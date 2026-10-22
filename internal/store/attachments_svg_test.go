package store

import (
	"strings"
	"testing"
)

func TestClassifySVG(t *testing.T) {
	t.Parallel()
	const ns = `xmlns="http://www.w3.org/2000/svg"`
	big := `<svg ` + ns + `>` + strings.Repeat("<g/>", 600000) + `</svg>`
	deep := `<svg ` + ns + `>` + strings.Repeat("<g>", 80) + strings.Repeat("</g>", 80) + `</svg>`
	many := `<svg ` + ns + `>` + strings.Repeat("<g/>", 60000) + `</svg>`
	cases := []struct {
		name        string
		svg         string
		image       bool
		width, high int
	}{
		{"plain", `<svg ` + ns + ` width="40" height="20"><rect width="40" height="20"/></svg>`, true, 40, 20},
		{"viewBox size", `<svg ` + ns + ` viewBox="0 0 120 60"><circle r="5"/></svg>`, true, 120, 60},
		{"px units", `<svg ` + ns + ` width="64px" height="32px"/>`, true, 64, 32},
		{"no size", `<svg ` + ns + `><g/></svg>`, true, 0, 0},
		{"xml declaration, comment, whitespace", "\n<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!-- made by a tool -->\n<svg " + ns + "/>", true, 0, 0},
		{"byte order mark", "\xef\xbb\xbf<svg " + ns + "/>", true, 0, 0},
		{"internal reference and style", `<svg ` + ns + ` xmlns:xlink="http://www.w3.org/1999/xlink"><defs><linearGradient id="g"/></defs><style>.a{fill:url(#g)}</style><use xlink:href="#g"/><rect style="fill:url(#g)"/></svg>`, true, 0, 0},
		{"inline png", `<svg ` + ns + `><image href="data:image/png;base64,AAAA"/></svg>`, true, 0, 0},
		{"no namespace", `<svg width="1"/>`, false, 0, 0},
		{"another root", `<html xmlns="http://www.w3.org/1999/xhtml"><svg ` + ns + `/></html>`, false, 0, 0},
		{"plain xml", `<?xml version="1.0"?><note><to>x</to></note>`, false, 0, 0},
		{"not xml", `hello <svg`, false, 0, 0},
		{"truncated", `<svg ` + ns + `><g>`, false, 0, 0},
		{"two roots", `<svg ` + ns + `/><svg ` + ns + `/>`, false, 0, 0},
		{"doctype", `<!DOCTYPE svg PUBLIC "-//W3C//DTD SVG 1.1//EN" "http://www.w3.org/Graphics/SVG/1.1/DTD/svg11.dtd"><svg ` + ns + `/>`, false, 0, 0},
		{"entity bomb", `<!DOCTYPE svg [<!ENTITY a "aaaa"><!ENTITY b "&a;&a;&a;&a;">]><svg ` + ns + `>&b;</svg>`, false, 0, 0},
		{"stylesheet instruction", `<?xml-stylesheet href="x.css"?><svg ` + ns + `/>`, false, 0, 0},
		{"script", `<svg ` + ns + `><script>alert(1)</script></svg>`, false, 0, 0},
		{"script, upper case", `<svg ` + ns + `><SCRIPT>x</SCRIPT></svg>`, false, 0, 0},
		{"foreignObject", `<svg ` + ns + `><foreignObject><div/></foreignObject></svg>`, false, 0, 0},
		{"event handler", `<svg ` + ns + ` onload="alert(1)"/>`, false, 0, 0},
		{"event handler on a child", `<svg ` + ns + `><rect OnClick="x()"/></svg>`, false, 0, 0},
		{"javascript url", `<svg ` + ns + `><a href="javascript:alert(1)"><rect/></a></svg>`, false, 0, 0},
		{"external link", `<svg ` + ns + `><a href="https://example.test/"><rect/></a></svg>`, false, 0, 0},
		{"external image", `<svg ` + ns + `><image href="https://example.test/p.png"/></svg>`, false, 0, 0},
		{"external use", `<svg ` + ns + ` xmlns:xlink="http://www.w3.org/1999/xlink"><use xlink:href="other.svg#a"/></svg>`, false, 0, 0},
		{"nested svg as data", `<svg ` + ns + `><image href="data:image/svg+xml;base64,AAAA"/></svg>`, false, 0, 0},
		{"css import", `<svg ` + ns + `><style>@import url(https://example.test/a.css);</style></svg>`, false, 0, 0},
		{"css url", `<svg ` + ns + `><style>.a{background:url(https://example.test/p.png)}</style></svg>`, false, 0, 0},
		{"css url in style attribute", `<svg ` + ns + `><rect style="fill:url('//example.test/x')"/></svg>`, false, 0, 0},
		{"css image-set", `<svg ` + ns + `><style>.a{background:image-set("https://example.test/p.png" 1x)}</style></svg>`, false, 0, 0},
		{"css webkit image-set", `<svg ` + ns + `><rect style="background:-webkit-image-set('x.png' 1x)"/></svg>`, false, 0, 0},
		{"css src function", `<svg ` + ns + `><style>@font-face{src:src("https://example.test/f")}</style></svg>`, false, 0, 0},
		{"css comment inside url", `<svg ` + ns + `><style>.a{background:url/**/(https://example.test/p.png)}</style></svg>`, false, 0, 0},
		{"css comment splits the name", `<svg ` + ns + `><style>.a{background:u/**/rl(https://example.test/p.png)}</style></svg>`, false, 0, 0},
		{"css comment inside import", `<svg ` + ns + `><style>@im/**/port "x.css";</style></svg>`, false, 0, 0},
		{"css comment in a style attribute", `<svg ` + ns + `><rect style="fill:url/*x*/(https://example.test/a)"/></svg>`, false, 0, 0},
		{"css split by an xml comment", `<svg ` + ns + `><style>.a{background:url(https://exa<!-- x -->mple.test/p.png)}</style></svg>`, false, 0, 0},
		{"css split by cdata", `<svg ` + ns + `><style>.a{background:ur<![CDATA[l(https://example.test/p.png)]]>}</style></svg>`, false, 0, 0},
		{"css fragment with a comment", `<svg ` + ns + `><defs><linearGradient id="g"/></defs><style>.a{fill:url/*c*/(#g)}</style></svg>`, true, 0, 0},
		{"css comment around a fragment", `<svg ` + ns + `><defs><linearGradient id="g"/></defs><style>/* a */.a{fill:url(#g)}/* b */</style></svg>`, true, 0, 0},
		{"animated href", `<svg ` + ns + `><a href="#x"><set attributeName="href" to="https://example.test/"/><rect/></a></svg>`, false, 0, 0},
		{"animated xlink href", `<svg ` + ns + ` xmlns:xlink="http://www.w3.org/1999/xlink"><use xlink:href="#a"><animate attributeName="xlink:href" values="#a;https://example.test/x"/></use></svg>`, false, 0, 0},
		{"animated href from and by", `<svg ` + ns + `><a><animate attributeName="href" from="#a" to="javascript:alert(1)"/></a></svg>`, false, 0, 0},
		{"animated style", `<svg ` + ns + `><rect><set attributeName="style" to="fill:url(https://example.test/a)"/></rect></svg>`, false, 0, 0},
		{"animated event handler", `<svg ` + ns + `><rect><set attributeName="onmouseover" to="x()"/></rect></svg>`, false, 0, 0},
		{"animated inside reference", `<svg ` + ns + `><a href="#x"><set attributeName="href" to="#y"/><rect/></a></svg>`, true, 0, 0},
		{"animated colour", `<svg ` + ns + `><rect><animate attributeName="fill" from="red" to="blue"/></rect></svg>`, true, 0, 0},
		{"iframe", `<svg ` + ns + `><iframe src="x"/></svg>`, false, 0, 0},
		{"too big", big, false, 0, 0},
		{"too deep", deep, false, 0, 0},
		{"too many tokens", many, false, 0, 0},
		{"empty", ``, false, 0, 0},
	}
	for _, c := range cases {
		got := ClassifySVG([]byte(c.svg))
		if got.Image != c.image || (c.image && (got.Width != c.width || got.Height != c.high)) {
			t.Errorf("%s: %+v, want image=%v %dx%d", c.name, got, c.image, c.width, c.high)
		}
	}
}
