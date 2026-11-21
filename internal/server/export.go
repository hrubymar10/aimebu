package server

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// exportPage is how many messages are read, and written, at a time: a long room is streamed in pages; only a small id-to-number map (one
// entry per message, to name the message a reply answers) grows with the room.
const exportPage = 500

var exportNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// exportFilename names the download after the room: letters, digits, dash and underscore only, so it is safe in a header and on any disk.
func exportFilename(title, format string) string {
	name := strings.Trim(exportNameUnsafe.ReplaceAllString(title, "-"), "-")
	if name == "" {
		name = "room"
	}
	if format == "markdown" {
		return name + ".md"
	}
	return name + ".json"
}

// GET /api/rooms/{room}/export?format=json|markdown downloads the whole history of a room, oldest first. Whoever may read the room
// may export it. JSON carries the room and every message as the messages route returns it, plus the reactions, the resolved author
// name and the number of the message a reply answers; Markdown is the same conversation written to be read: replies and threads
// are named, attachments listed (their names and sizes, not their bytes), reactions summed per emoji.
func (s *Server) exportRoom(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomAccess(w, r, r.PathValue("room"))
	if !ok {
		return
	}
	format := r.URL.Query().Get("format")
	if format != "json" && format != "markdown" {
		apiError(w, 400, "invalid_request", "format must be json or markdown")
		return
	}
	project, err := s.Store.ProjectByID(r.Context(), room.ProjectID)
	if err != nil {
		fail(w, err)
		return
	}
	s.exportMu.RLock()
	page, size := s.exportPageFn, exportPage
	if s.exportPageSize > 0 {
		size = s.exportPageSize
	}
	s.exportMu.RUnlock()
	if page == nil {
		page = s.Store.ListMessagesAfterLimit
	}
	// The first page is read before anything is sent, so a failure there is an ordinary error response. A failure later cannot change the
	// status that has gone out, so it ends the connection abruptly (below): a client then sees a broken download, never a file that looks whole.
	messages, err := page(r.Context(), room.ID, 0, size)
	if err != nil {
		fail(w, err)
		return
	}
	names := &exportNames{ctx: r.Context(), store: s.Store, project: project.Name, byID: map[string]string{}}
	numbers := map[string]int64{}  // message id -> number, to name the message a reply answers (one small entry per message)
	var snippets map[string]string // Markdown only: a line of each message, to quote it in the replies to it
	if format == "markdown" {
		snippets = map[string]string{}
		names.byMsg = map[string]string{}
	}
	contentType := "application/json"
	if format == "markdown" {
		contentType = "text/markdown; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, exportFilename(room.Title, format)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	out := &exportWriter{w: w}
	exported := time.Now().UTC().Format(time.RFC3339)
	if format == "json" {
		roomJSON, err := json.Marshal(room)
		if err != nil {
			fail(w, err)
			return
		}
		fmt.Fprintf(out, `{"room":%s,"exported_at":%q,"messages":[`, roomJSON, exported)
	} else {
		fmt.Fprintf(out, "# %s\n\n_Exported %s from %s._\n", markdownLine(room.Title), exported, markdownLine(project.Name))
	}
	first := true
	for len(messages) > 0 {
		if r.Context().Err() != nil || out.err != nil {
			return // the reader went away: nothing more to write, and nobody to tell
		}
		reactions, err := s.Store.ReactionsForMessages(r.Context(), messages)
		if err != nil {
			panic(http.ErrAbortHandler)
		}
		var threadCounts map[string]int
		var groupOnly map[string]bool
		if format == "json" {
			ids := make([]string, 0, len(messages))
			for _, message := range messages {
				ids = append(ids, message.ID)
			}
			if threadCounts, err = s.Store.ThreadCountsForMessages(r.Context(), room.ID, messages); err != nil {
				panic(http.ErrAbortHandler)
			}
			if groupOnly, err = s.Store.GroupOnlyMessages(r.Context(), ids); err != nil {
				panic(http.ErrAbortHandler)
			}
		}
		for _, message := range messages {
			for i := range message.Attachments {
				message.Attachments[i].Path = "" // the file's place on the server is not part of an export
			}
			numbers[message.ID] = message.No
			author := names.of(message)
			parentNo := int64(0)
			if message.ReplyTo != "" {
				parentNo = numbers[message.ReplyTo]
			}
			if format == "json" {
				view := messageView(message)
				view["expirable"] = store.ExpirableMessage(message.AuthorKind, message.ReplyTo, room.Kind, groupOnly[message.ID])
				items := reactions[message.ID]
				if items == nil {
					items = []map[string]string{}
				}
				view["reactions"] = items
				rootID := message.ThreadRoot
				if rootID == "" {
					rootID = message.ID
				}
				view["thread_count"] = threadCounts[rootID]
				view["author_label"] = author
				if parentNo > 0 {
					view["reply_to_no"] = parentNo
				}
				encoded, err := json.Marshal(view)
				if err != nil {
					panic(http.ErrAbortHandler)
				}
				if !first {
					out.Write([]byte(","))
				}
				out.Write(encoded)
			} else {
				snippets[message.ID] = store.MessageSnippet(message.Body, len(message.Attachments))
				parentAuthor := ""
				if parentNo > 0 {
					parentAuthor = names.ofID(message.ReplyTo)
				}
				out.Write([]byte(exportMarkdownMessage(message, author, parentNo, parentAuthor, snippets[message.ReplyTo], numbers[message.ThreadRoot], reactions[message.ID])))
				names.remember(message.ID, author)
			}
			first = false
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush() // each page goes out as it is written: a slow reader sees progress, and a failure later breaks a download already begun
		}
		after := messages[len(messages)-1].Seq
		if len(messages) < size {
			break
		}
		if messages, err = page(r.Context(), room.ID, after, size); err != nil {
			panic(http.ErrAbortHandler)
		}
	}
	if out.err != nil {
		return
	}
	if format == "json" {
		out.Write([]byte("]}\n"))
	}
}

// exportWriter remembers the first write error, so a gone reader is noticed once per page rather than ignored message by message.
type exportWriter struct {
	w   http.ResponseWriter
	err error
}

func (e *exportWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	e.err = err
	return n, err
}

// setExportHooks lets a test make the export's pages small and make reading one fail; the handler reads them under the same lock.
func (s *Server) setExportHooks(size int, page func(ctx context.Context, roomID string, after int64, limit int) ([]store.Message, error)) {
	s.exportMu.Lock()
	s.exportPageSize, s.exportPageFn = size, page
	s.exportMu.Unlock()
}

// exportNames turns an author into the name a reader knows: a person's login, an agent's slug@project, "System", and for an agent whose record
// is gone the readable id an import kept (or a short form of an opaque one), as the chat shows them.
type exportNames struct {
	ctx      context.Context
	store    *store.Store
	project  string
	byID     map[string]string // author id -> name
	byMsg    map[string]string // message id -> author name, for naming the author of a reply's parent
	projects map[string]string // project id -> name, for an author that belonged to another project than the room's (an agent chat)
}

// projectNamed is the name of a project by its id, the fallback when it cannot be read.
func (n *exportNames) projectNamed(id, fallback string) string {
	if name, ok := n.projects[id]; ok {
		return name
	}
	name := fallback
	if project, err := n.store.ProjectByID(n.ctx, id); err == nil && project.Name != "" {
		name = project.Name
	}
	if n.projects == nil {
		n.projects = map[string]string{}
	}
	n.projects[id] = name
	return name
}

func (n *exportNames) of(message store.Message) string {
	switch message.AuthorKind {
	case "system":
		return "System"
	case "user":
		if message.AuthorName != "" {
			return message.AuthorName
		}
		return message.AuthorID
	}
	if name, ok := n.byID[message.AuthorID]; ok {
		return name
	}
	if message.AuthorName != "" { // an agent that was removed: named as a live one is, with the project it belonged to, and said so
		name, project := message.AuthorName, n.project
		if message.AuthorProjectID != "" {
			project = n.projectNamed(message.AuthorProjectID, project)
		}
		if project != "" {
			name += "@" + project
		}
		return name + " (removed)"
	}
	name := message.AuthorID
	if card, err := n.store.AgentCardByID(n.ctx, message.AuthorID); err == nil && card.Slug != "" {
		name = card.Slug
		if n.project != "" {
			name += "@" + n.project
		}
	} else if strings.HasPrefix(message.AuthorID, "agt_") && len(message.AuthorID) > 6 {
		name = "Agent " + message.AuthorID[len(message.AuthorID)-6:]
	}
	n.byID[message.AuthorID] = name
	return name
}

func (n *exportNames) remember(messageID, name string) {
	if n.byMsg == nil {
		n.byMsg = map[string]string{}
	}
	n.byMsg[messageID] = name
}

func (n *exportNames) ofID(messageID string) string { return n.byMsg[messageID] }

// markdownLine makes text safe to put on one Markdown line: no line breaks, and the characters that would start a heading or a quote are
// left alone because the text is never first on its line.
func markdownLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func exportSize(bytes int64) string {
	switch {
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(bytes)/(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(bytes)/(1<<10))
	}
	return fmt.Sprintf("%d B", bytes)
}

func exportMarkdownMessage(message store.Message, author string, parentNo int64, parentAuthor, parentSnippet string, rootNo int64, reactions []map[string]string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "\n## #%d · %s · %s\n\n", message.No, markdownLine(author), message.CreatedAt)
	if parentNo > 0 {
		fmt.Fprintf(&out, "> ↪ in reply to #%d", parentNo)
		if parentAuthor != "" {
			fmt.Fprintf(&out, " %s", markdownLine(parentAuthor))
		}
		if parentSnippet != "" {
			fmt.Fprintf(&out, ": %s", parentSnippet)
		}
		out.WriteString("\n\n")
	}
	if rootNo > 0 && rootNo != parentNo {
		fmt.Fprintf(&out, "_In the thread of #%d._\n\n", rootNo)
	}
	out.WriteString(strings.TrimRight(message.Body, "\n"))
	out.WriteString("\n")
	for _, attachment := range message.Attachments {
		name := markdownLine(attachment.Name)
		if name == "" {
			name = "attachment"
		}
		if attachment.RemovedAt != "" {
			fmt.Fprintf(&out, "\n- 📎 %s (attachment removed)", name)
		} else {
			fmt.Fprintf(&out, "\n- 📎 %s (%s, %s)", name, attachment.MIME, exportSize(attachment.Size))
		}
	}
	if len(message.Attachments) > 0 {
		out.WriteString("\n")
	}
	if len(reactions) > 0 {
		perEmoji := map[string][]string{}
		for _, item := range reactions {
			perEmoji[item["emoji"]] = append(perEmoji[item["emoji"]], item["member_name"])
		}
		emojis := make([]string, 0, len(perEmoji))
		for emoji := range perEmoji {
			emojis = append(emojis, emoji)
		}
		sort.Strings(emojis)
		parts := make([]string, 0, len(emojis))
		for _, emoji := range emojis {
			parts = append(parts, emoji+" "+strings.Join(perEmoji[emoji], ", "))
		}
		fmt.Fprintf(&out, "\nReactions: %s\n", strings.Join(parts, " · "))
	}
	return out.String()
}
