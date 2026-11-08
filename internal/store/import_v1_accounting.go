package store

import (
	"fmt"
	"sort"
)

// V1AccountingGroup reconciles one count of the backup (its rooms, messages, attachments or reactions) to what the import does with it:
// the parts add up to Total, so nothing is dropped without a line saying why.
type V1AccountingGroup struct {
	Key   string             `json:"key"`
	Total int                `json:"total"`
	Parts []V1AccountingPart `json:"parts"`
}

// V1AccountingPart is one fate of those records. Key is "imported", "already_present", a skip reason (see v1SkipReasonRoom and friends) or "other"
// (what no named reason accounts for); Items name what the owner can look at (room ids with their message counts), never message text.
type V1AccountingPart struct {
	Key   string   `json:"key"`
	Count int      `json:"count"`
	Items []string `json:"items,omitempty"`
}

// The reasons a v1 room is not imported. Its messages, attachments and reactions share the reason.
const (
	v1SkipSystemRoom = "system_room"     // v1's own event room
	v1SkipDM         = "dm_not_imported" // a direct message the owner did not tick
	v1SkipNoProject  = "no_project"      // a room with no project to go into
	v1SkipChosen     = "room_skipped"    // a room the owner chose to leave out
	v1SkipPresence   = "presence_lines"  // v1's join/leave and memory bookkeeping lines
	v1SkipPerson     = "person_skipped"  // a reaction by a person mapped to nobody
)

func v1RoomSkipReason(data *v1ImportData, mapping V1ImportMap, noProject map[string]bool, roomID string) string {
	switch {
	case roomID == V1SystemRoom:
		return v1SkipSystemRoom
	case noProject[roomID]:
		return v1SkipNoProject
	case isV1DM(roomID):
		return v1SkipDM
	}
	return v1SkipChosen
}

// buildV1Accounting computes the accounting from the backup, the mapping and the counts the import (or its dry run) just made. Pure: it reads
// nothing but its arguments.
func buildV1Accounting(data *v1ImportData, mapping V1ImportMap, report V1ImportReport) []V1AccountingGroup {
	noProject := map[string]bool{}
	for _, id := range report.SkippedNoProject {
		noProject[id] = true
	}
	reasonOf := map[string]string{}
	for _, room := range data.Rooms {
		if skippedV1Room(mapping, room.ID) {
			reasonOf[room.ID] = v1RoomSkipReason(data, mapping, noProject, room.ID)
		}
	}
	// per reason, per room
	messages := map[string]map[string]int{}
	reactionsBy := map[string]int{}
	add := func(by map[string]map[string]int, reason, room string) {
		if by[reason] == nil {
			by[reason] = map[string]int{}
		}
		by[reason][room]++
	}
	// What happens to each attachment, by the messages that refer to it: one that an imported message refers to is imported, whatever else
	// refers to it; otherwise it is left out with the reason of the message (a skipped room, a presence line); one nothing refers to is unused.
	attachmentFate := map[string]string{}
	keep := func(id, fate string) {
		if _, seen := attachmentFate[id]; !seen || fate == "imported" {
			attachmentFate[id] = fate
		}
	}
	for _, message := range data.Messages {
		fate := "imported"
		switch {
		case skippedV1Room(mapping, message.RoomID):
			fate = reasonOf[message.RoomID]
			add(messages, fate, message.RoomID)
			reactionsBy[fate] += len(data.Reactions[message.ID])
		case isV1PresenceLine(message):
			fate = v1SkipPresence
			add(messages, fate, message.RoomID)
			reactionsBy[fate] += len(data.Reactions[message.ID])
		default:
			for _, reaction := range data.Reactions[message.ID] {
				if v1ImportKind(reaction.AgentID, data.Agents) == "user" && skippedV1Human(mapping, reaction.AgentID) {
					reactionsBy[v1SkipPerson]++
				}
			}
		}
		for _, ref := range message.Attachments {
			keep(ref.ID, fate)
		}
	}
	itemsOf := func(by map[string]int) []string {
		ids := make([]string, 0, len(by))
		for id := range by {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		items := make([]string, 0, len(ids))
		for _, id := range ids {
			items = append(items, fmt.Sprintf("%s · %d", id, by[id]))
		}
		return items
	}
	order := []string{v1SkipPresence, v1SkipSystemRoom, v1SkipDM, v1SkipNoProject, v1SkipChosen, v1SkipPerson}
	finish := func(group V1AccountingGroup) V1AccountingGroup {
		sum := 0
		for _, part := range group.Parts {
			sum += part.Count
		}
		if rest := group.Total - sum; rest != 0 {
			group.Parts = append(group.Parts, V1AccountingPart{Key: "other", Count: rest})
		}
		return group
	}
	var groups []V1AccountingGroup

	rooms := V1AccountingGroup{Key: "rooms", Total: len(data.Rooms)}
	rooms.Parts = append(rooms.Parts, V1AccountingPart{Key: "imported", Count: report.Imported["rooms"]})
	if n := report.AlreadyPresent["rooms"]; n > 0 {
		rooms.Parts = append(rooms.Parts, V1AccountingPart{Key: "already_present", Count: n})
	}
	skippedRooms := map[string][]string{}
	for _, room := range data.Rooms {
		if reason, ok := reasonOf[room.ID]; ok {
			skippedRooms[reason] = append(skippedRooms[reason], room.ID)
		}
	}
	for _, reason := range order {
		if ids := skippedRooms[reason]; len(ids) > 0 {
			sort.Strings(ids)
			rooms.Parts = append(rooms.Parts, V1AccountingPart{Key: reason, Count: len(ids), Items: ids})
		}
	}
	groups = append(groups, finish(rooms))

	msgs := V1AccountingGroup{Key: "messages", Total: len(data.Messages)}
	msgs.Parts = append(msgs.Parts, V1AccountingPart{Key: "imported", Count: report.Imported["messages"]})
	if n := report.AlreadyPresent["messages"]; n > 0 {
		msgs.Parts = append(msgs.Parts, V1AccountingPart{Key: "already_present", Count: n})
	}
	for _, reason := range order {
		by := messages[reason]
		if len(by) == 0 {
			continue
		}
		count := 0
		for _, n := range by {
			count += n
		}
		msgs.Parts = append(msgs.Parts, V1AccountingPart{Key: reason, Count: count, Items: itemsOf(by)})
	}
	groups = append(groups, finish(msgs))

	attachments := V1AccountingGroup{Key: "attachments", Total: len(data.Attachments)}
	attachments.Parts = append(attachments.Parts, V1AccountingPart{Key: "imported", Count: report.Imported["attachments"]})
	if n := report.AlreadyPresent["attachments"]; n > 0 {
		attachments.Parts = append(attachments.Parts, V1AccountingPart{Key: "already_present", Count: n})
	}
	left := map[string]int{}
	for id := range data.Attachments {
		if fate, ok := attachmentFate[id]; !ok {
			left["unused_files"]++
		} else if fate != "imported" {
			left[fate]++
		}
	}
	for _, reason := range append(append([]string{}, order...), "unused_files") {
		if n := left[reason]; n > 0 {
			key := reason
			if reason != v1SkipPresence && reason != "unused_files" {
				key = "in_skipped_rooms"
			}
			attachments.Parts = mergeAccountingPart(attachments.Parts, key, n)
		}
	}
	groups = append(groups, finish(attachments))

	total := 0
	for _, list := range data.Reactions {
		total += len(list)
	}
	reactions := V1AccountingGroup{Key: "reactions", Total: total}
	reactions.Parts = append(reactions.Parts, V1AccountingPart{Key: "imported", Count: report.Imported["reactions"]})
	if n := report.AlreadyPresent["reactions"]; n > 0 {
		reactions.Parts = append(reactions.Parts, V1AccountingPart{Key: "already_present", Count: n})
	}
	for _, reason := range order {
		if n := reactionsBy[reason]; n > 0 {
			reactions.Parts = append(reactions.Parts, V1AccountingPart{Key: reason, Count: n})
		}
	}
	groups = append(groups, finish(reactions))
	return groups
}

// mergeAccountingPart adds count to the part named key, making the part if there is none yet.
func mergeAccountingPart(parts []V1AccountingPart, key string, count int) []V1AccountingPart {
	for i := range parts {
		if parts[i].Key == key {
			parts[i].Count += count
			return parts
		}
	}
	return append(parts, V1AccountingPart{Key: key, Count: count})
}

// v1UnreferencedAttachments counts the attachments of the backup that no message refers to.
func v1UnreferencedAttachments(data *v1ImportData) int {
	referenced := map[string]bool{}
	for _, message := range data.Messages {
		for _, ref := range message.Attachments {
			referenced[ref.ID] = true
		}
	}
	count := 0
	for id := range data.Attachments {
		if !referenced[id] {
			count++
		}
	}
	return count
}
