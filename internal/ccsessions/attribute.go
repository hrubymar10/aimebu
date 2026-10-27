package ccsessions

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

// Attribution is what the aimebu tool results in one transcript say about whose session it was: the agent id its bus_register returned and
// the numbers of the messages its bus_say and bus_dm sent. Nothing else of the transcript is kept, and none of it is text.
type Attribution struct {
	SessionID string
	CWD       string
	Model     string
	// Sender is the agent id of the last registration in the session ("yann@aimebu").
	Sender string
	// MessageNos are the numbers of the messages the session sent, oldest first; only the newest MaxMessageNos are kept.
	MessageNos []int64
	// ModifiedAt is when the transcript was last written.
	ModifiedAt time.Time
}

const (
	// MaxMessageNos bounds how many message numbers one session reports.
	MaxMessageNos = 2000
	// MaxAttributeFile and MaxAttributeBytes bound what attribution reads: one transcript, and one scan in all. A transcript over the first
	// is left out (and the scan marked truncated); the second ends the scan.
	MaxAttributeFile  = 512 << 20
	MaxAttributeBytes = 2 << 30
	// MaxAttributeLine is the longest transcript line looked at; a longer one (a huge tool result) is skipped, never parsed.
	MaxAttributeLine = 32 << 20
)

// AttributionScan is the result of Attribute.
type AttributionScan struct {
	Sessions  []Attribution
	Truncated bool
}

// Attribute reads, for the sessions that worked in one of the folders (see Scan) and were written at or after since, which agent each one
// registered as and which message numbers it sent, so a v1 message number can be traced to the session that sent it. Only sessions whose last
// registration is agent are returned when agent is not empty. It reads whole transcripts, line by line, and keeps only ids: no message or tool
// text leaves this function. deadline bounds the whole scan (zero for none).
func Attribute(configDir string, folders []string, since time.Time, agent string, deadline time.Time) (AttributionScan, error) {
	found, err := Scan(configDir, folders...)
	if err != nil {
		return AttributionScan{}, err
	}
	out := AttributionScan{Truncated: found.Truncated}
	var total int64
	for _, session := range found.Sessions {
		if !since.IsZero() && session.ModifiedAt.Before(since) {
			continue
		}
		if session.Size > MaxAttributeFile {
			out.Truncated = true
			continue
		}
		if total += session.Size; total > MaxAttributeBytes || (!deadline.IsZero() && time.Now().After(deadline)) {
			out.Truncated = true
			break
		}
		attribution, err := attributeTranscript(filepath.Join(configDir, "projects", session.Dir, session.ID+".jsonl"), deadline)
		if errors.Is(err, errAttributeDeadline) {
			out.Truncated = true
			break
		}
		if err != nil || attribution.Sender == "" || (agent != "" && attribution.Sender != agent) {
			continue
		}
		attribution.SessionID, attribution.CWD, attribution.Model, attribution.ModifiedAt = session.ID, session.CWD, session.Model, session.ModifiedAt
		out.Sessions = append(out.Sessions, attribution)
	}
	return out, nil
}

var errAttributeDeadline = errors.New("attribution deadline")

// attributedTools are the aimebu tools whose results name an agent or a message. Claude names an MCP tool mcp__<server>__<tool>, and the
// server's name is the user's, so only the tool's own name is matched.
var attributedTools = map[string]string{"bus_register": "register", "bus_say": "message", "bus_dm": "message"}

func attributeTranscript(path string, deadline time.Time) (Attribution, error) {
	file, err := os.Open(path)
	if err != nil {
		return Attribution{}, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 1<<20)
	var out Attribution
	pending := map[string]string{} // tool_use id -> "register" | "message"
	checked := 0
	for {
		raw, tooLong, err := readLine(reader)
		if len(raw) > 0 && !tooLong {
			if checked++; checked%512 == 0 && !deadline.IsZero() && time.Now().After(deadline) {
				return Attribution{}, errAttributeDeadline
			}
			attributeLine(raw, pending, &out)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return Attribution{}, err
		}
	}
}

// readLine returns the next line; a line longer than MaxAttributeLine is read through and reported as tooLong with no content.
func readLine(reader *bufio.Reader) (line []byte, tooLong bool, err error) {
	for {
		part, err := reader.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(part) > MaxAttributeLine {
				line, tooLong = nil, true
			} else {
				line = append(line, part...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return bytes.TrimRight(line, "\r\n"), tooLong, err
	}
}

func attributeLine(raw []byte, pending map[string]string, out *Attribution) {
	switch {
	case bytes.Contains(raw, []byte(`"tool_use"`)) && (bytes.Contains(raw, []byte("bus_register")) || bytes.Contains(raw, []byte("bus_say")) || bytes.Contains(raw, []byte("bus_dm"))):
		var l struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		var parts []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &l) != nil || json.Unmarshal(l.Message.Content, &parts) != nil {
			return
		}
		for _, part := range parts {
			if part.Type != "tool_use" || part.ID == "" {
				continue
			}
			name := part.Name
			if i := strings.LastIndex(name, "__"); i >= 0 {
				name = name[i+2:]
			}
			if kind := attributedTools[name]; kind != "" {
				pending[part.ID] = kind
			}
		}
	case len(pending) > 0 && bytes.Contains(raw, []byte(`"tool_result"`)):
		var l struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		var parts []struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &l) != nil || json.Unmarshal(l.Message.Content, &parts) != nil {
			return
		}
		for _, part := range parts {
			kind := pending[part.ToolUseID]
			if part.Type != "tool_result" || kind == "" {
				continue
			}
			delete(pending, part.ToolUseID)
			var result struct {
				ID json.RawMessage `json:"id"`
			}
			if json.Unmarshal([]byte(resultText(part.Content)), &result) != nil {
				continue
			}
			switch kind {
			case "register":
				var id string
				if json.Unmarshal(result.ID, &id) == nil && id != "" {
					out.Sender = id
				}
			case "message":
				var no int64
				if json.Unmarshal(result.ID, &no) == nil && no > 0 {
					if len(out.MessageNos) == MaxMessageNos {
						out.MessageNos = append(out.MessageNos[:0], out.MessageNos[1:]...)
					}
					out.MessageNos = append(out.MessageNos, no)
				}
			}
		}
	}
}

// resultText is the text of a tool result: Claude writes it as a string or as a list of typed parts.
func resultText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, part := range parts {
		if part.Type == "text" {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}
