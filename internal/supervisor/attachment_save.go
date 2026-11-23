package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/agenttools"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// attachmentFetchWait bounds the download of one attachment (at most agenttools.MaxAttachmentBytes) from the server.
const attachmentFetchWait = 90 * time.Second

// saveAttachment answers an attachment_save frame. It saves only for the turn the agent runs here now: the frame's turn must be the agent's
// active, ordinary turn on this machine, so a turn of another agent, or one that has ended, is refused. The bytes come from the server's
// agent attachment route under the agent's own key, which applies the same checks as attachment_get (a note delivered in that live turn,
// room membership still held). The file goes into the turn's checkout through agenttools.SaveAttachmentLikeCheckout (name cleaning, no
// linked folder, an atomic write, the 10 MB limit; the checkout's owner and its read access, so a container user other than this process's
// can read it), and the answer is the path as the agent sees it: mapped through its launcher's path map when it has a launcher, as its
// checkout is.
func (c *Client) saveAttachment(ctx context.Context, frame protocol.Frame) protocol.Frame {
	result := protocol.Frame{V: protocol.Version, Type: protocol.AttachmentSaveResult, ReqID: frame.ReqID, AgentID: frame.AgentID, TurnID: frame.TurnID, AttachmentID: frame.AttachmentID}
	fail := func(code, message string) protocol.Frame {
		result.Code, result.Message = code, message
		return result
	}
	c.mu.Lock()
	active := c.active[frame.AgentID]
	info := c.activeTurns[frame.AgentID]
	key := c.agentKeys[frame.AgentID]
	c.mu.Unlock()
	if frame.AgentID == "" || frame.TurnID == "" || active != frame.TurnID || info.turnID != frame.TurnID || info.mode != adapter.ModeNormal {
		return fail("turn_not_running", "the agent runs no such turn on this machine")
	}
	if info.checkout == "" {
		return fail("no_workspace", "the turn has no checkout on this machine")
	}
	if key == "" {
		return fail("no_key", "this machine holds no key for the agent")
	}
	data, name, mimeType, err := c.fetchAttachment(ctx, key, frame.AttachmentID)
	if err != nil {
		return fail("fetch_failed", err.Error())
	}
	hostPath, err := agenttools.SaveAttachmentLikeCheckout(info.checkout, frame.AttachmentID, name, data)
	if err != nil {
		return fail("save_failed", err.Error())
	}
	visible := hostPath
	if info.launcherID != "" {
		row := c.launcherRow(info.launcherID)
		if row.ID == "" {
			return fail("save_failed", "the agent's launcher is gone from this machine")
		}
		if visible, err = launcherCheckout(row.LauncherDefinition, hostPath); err != nil {
			return fail("save_failed", fmt.Sprintf("the saved file has no path the agent can see: %v", err))
		}
	}
	result.Details, _ = json.Marshal(protocol.AttachmentSaved{Path: visible, MIME: mimeType, Size: len(data)})
	return result
}

// fetchAttachment downloads one attachment with the agent's key: its bytes, its stored name and its type.
func (c *Client) fetchAttachment(ctx context.Context, key, attachmentID string) ([]byte, string, string, error) {
	if !strings.HasPrefix(attachmentID, "att_") || len(attachmentID) > 100 {
		return nil, "", "", errors.New("invalid attachment_id")
	}
	ctx, cancel := context.WithTimeout(ctx, attachmentFetchWait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.ServerURL, "/")+"/api/agent/attachments/"+url.PathEscape(attachmentID), nil)
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, agenttools.MaxAttachmentBytes+1))
	if err != nil {
		return nil, "", "", err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", fmt.Errorf("the server answered HTTP %d", resp.StatusCode)
	}
	if len(data) > agenttools.MaxAttachmentBytes {
		return nil, "", "", fmt.Errorf("attachment is larger than %d MB", agenttools.MaxAttachmentBytes>>20)
	}
	mimeType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return data, agenttools.AttachmentFileName(resp.Header.Get("Content-Disposition")), mimeType, nil
}
