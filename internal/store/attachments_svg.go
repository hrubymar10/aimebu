package store

import (
	"bytes"
	"io"
	"strconv"
	"strings"

	"encoding/xml"
)

// An SVG is an image people may attach, but it is also a document that can carry script and reach out to other servers. So it is
// classified, never rewritten: a file that is plainly a still image becomes image/svg+xml and is shown only through an <img>; one that is
// not valid SVG, is too big, or carries anything active keeps the type sniffing gave it (plain text or XML) and stays download-only,
// exactly as every SVG was before.
const (
	// SVGMaxBytes is the largest file treated as an SVG image (every attachment is still capped at MaxAttachmentSize).
	SVGMaxBytes  = 2 << 20
	svgMaxTokens = 50000
	svgMaxDepth  = 64
	svgNamespace = "http://www.w3.org/2000/svg"
	xlinkNS      = "http://www.w3.org/1999/xlink"
)

// SVGInfo is what classifying a file found.
type SVGInfo struct {
	// Image is true for valid SVG with nothing active in it, within the size and complexity limits.
	Image bool
	// Width and Height are the image's size in pixels when its width/height or viewBox says, else 0.
	Width, Height int
}

// activeElements run code, embed other documents or play media: an SVG with one is not an image to us.
var activeElements = map[string]bool{
	"script": true, "foreignobject": true, "iframe": true, "embed": true, "object": true, "audio": true, "video": true, "handler": true, "listener": true,
}

// allowedReference reports whether a link or resource reference stays inside the file or is an inline raster image.
func allowedReference(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "#") {
		return true
	}
	lower := strings.ToLower(value)
	for _, prefix := range []string{"data:image/png", "data:image/jpeg", "data:image/gif", "data:image/webp"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// stripCSSComments removes /* */ comments (an unterminated one runs to the end, as a browser reads it), so a comment cannot split a token
// such as url( or @import from the checks below.
func stripCSSComments(css string) string {
	var out strings.Builder
	for {
		at := strings.Index(css, "/*")
		if at < 0 {
			out.WriteString(css)
			return out.String()
		}
		out.WriteString(css[:at])
		end := strings.Index(css[at+2:], "*/")
		if end < 0 {
			return out.String()
		}
		css = css[at+2+end+2:]
	}
}

// activeStyle reports whether CSS text reaches outside the file: an @import, an image-set() or src() function (they take a bare string
// that names a file), or a url() that is not a fragment or an inline raster image.
func activeStyle(css string) bool {
	lower := strings.ToLower(stripCSSComments(css))
	for _, word := range []string{"@import", "expression(", "javascript:", "image-set(", "src("} {
		if strings.Contains(lower, word) {
			return true
		}
	}
	for rest := lower; ; {
		at := strings.Index(rest, "url(")
		if at < 0 {
			return false
		}
		rest = rest[at+4:]
		end := strings.IndexByte(rest, ')')
		if end < 0 {
			return true
		}
		target := strings.Trim(strings.TrimSpace(rest[:end]), `"'`)
		if !allowedReference(target) {
			return true
		}
		rest = rest[end:]
	}
}

// activeAnimation reports whether an animation element (set, animate) can write something that reaches outside the file: it names an
// event handler as its target, or the value it writes (to, from, by, values) is a reference or CSS that leaves the file. A value written
// to href must pass the same check as the attribute itself.
func activeAnimation(attrs []xml.Attr) bool {
	var target string
	for _, attr := range attrs {
		if attr.Name.Local == "attributeName" {
			target = strings.ToLower(strings.TrimSpace(attr.Value))
			if i := strings.IndexByte(target, ':'); i >= 0 {
				target = target[i+1:]
			}
		}
	}
	if strings.HasPrefix(target, "on") {
		return true
	}
	for _, attr := range attrs {
		switch attr.Name.Local {
		case "to", "from", "by", "values":
			for _, value := range strings.Split(attr.Value, ";") {
				if activeStyle(value) || ((target == "href" || target == "src") && !allowedReference(value)) {
					return true
				}
			}
		}
	}
	return false
}

func svgLength(value string) int {
	value = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(value), "px"))
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || number < 1 || number > 20000 {
		return 0
	}
	return int(number + 0.5)
}

// ClassifySVG reads data as an SVG with the standard library's strict XML decoder: UTF-8 only, no DOCTYPE or entity declarations, bounded
// size, tokens and depth, the root an svg element in the SVG namespace, and nothing active anywhere in it (see activeElements, on*
// attributes, references that leave the file, CSS that fetches).
func ClassifySVG(data []byte) SVGInfo {
	if len(data) == 0 || len(data) > SVGMaxBytes {
		return SVGInfo{}
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.Strict = true
	decoder.CharsetReader = nil
	var info SVGInfo
	depth, tokens, root, finished := 0, 0, false, false
	var styleDepth []bool
	var styleText strings.Builder // the text of the open <style>, all of it: comments and CDATA split it into several tokens
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return SVGInfo{}
		}
		tokens++
		if tokens > svgMaxTokens {
			return SVGInfo{}
		}
		switch t := token.(type) {
		case xml.Directive:
			return SVGInfo{}
		case xml.ProcInst:
			if t.Target != "xml" && t.Target != "xml-stylesheet" {
				return SVGInfo{}
			}
			if t.Target == "xml-stylesheet" {
				return SVGInfo{}
			}
		case xml.StartElement:
			if finished { // a second root element
				return SVGInfo{}
			}
			depth++
			if depth > svgMaxDepth {
				return SVGInfo{}
			}
			if !root {
				if t.Name.Local != "svg" || t.Name.Space != svgNamespace {
					return SVGInfo{}
				}
				root = true
				for _, attr := range t.Attr {
					switch attr.Name.Local {
					case "width":
						info.Width = svgLength(attr.Value)
					case "height":
						info.Height = svgLength(attr.Value)
					}
				}
				if info.Width == 0 || info.Height == 0 {
					for _, attr := range t.Attr {
						if attr.Name.Local == "viewBox" {
							if fields := strings.Fields(strings.ReplaceAll(attr.Value, ",", " ")); len(fields) == 4 {
								w, h := svgLength(fields[2]), svgLength(fields[3])
								if info.Width == 0 && info.Height == 0 {
									info.Width, info.Height = w, h
								} else if info.Width == 0 {
									info.Width = w
								} else {
									info.Height = h
								}
							}
						}
					}
				}
				if info.Width == 0 || info.Height == 0 {
					info.Width, info.Height = 0, 0
				}
			}
			if activeElements[strings.ToLower(t.Name.Local)] {
				return SVGInfo{}
			}
			if name := strings.ToLower(t.Name.Local); (name == "set" || name == "animate") && activeAnimation(t.Attr) {
				return SVGInfo{}
			}
			for _, attr := range t.Attr {
				name := strings.ToLower(attr.Name.Local)
				if strings.HasPrefix(name, "on") || strings.Contains(strings.ToLower(attr.Value), "javascript:") {
					return SVGInfo{}
				}
				if name == "href" || name == "src" {
					if !allowedReference(attr.Value) {
						return SVGInfo{}
					}
				}
				if name == "style" && activeStyle(attr.Value) {
					return SVGInfo{}
				}
			}
			styleDepth = append(styleDepth, strings.EqualFold(t.Name.Local, "style"))
		case xml.EndElement:
			depth--
			finished = depth == 0
			if styleDepth[len(styleDepth)-1] {
				if activeStyle(styleText.String()) {
					return SVGInfo{}
				}
				styleText.Reset()
			}
			styleDepth = styleDepth[:len(styleDepth)-1]
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(t)) > 0 { // text outside the root
				return SVGInfo{}
			}
			if len(styleDepth) > 0 && styleDepth[len(styleDepth)-1] {
				styleText.Write(t)
			}
		}
	}
	if !root || depth != 0 {
		return SVGInfo{}
	}
	info.Image = true
	return info
}
