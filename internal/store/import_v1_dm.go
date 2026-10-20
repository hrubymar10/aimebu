package store

import (
	"strings"
)

// A v1 DM ("dm:a:b") becomes a v2 direct message: kind dm, listed under Direct messages and titled by the other participant, as
// a DM v2 opens itself would be (an agent's slug). Its dm_key stays the legacy key so it can never collide with a live pair.
// Two things differ from a task room: a DM has no archive in v2's sidebar (an archived DM would be unreachable), so it always
// imports open, and it belongs to a project only because every v2 room does. In the wizard the owner is always a member, which
// makes a DM between two AIs visible to the owner; its other participants are historical names, so it reads as history.

func isV1DM(roomID string) bool { return strings.HasPrefix(roomID, "dm:") }

// v1DMMembers is the DM's participants: its member list, or the two ids in its name when the list is empty.
func v1DMMembers(room v1ImportRoom) []string {
	if len(room.Members) > 0 {
		return room.Members
	}
	parts := strings.Split(strings.TrimPrefix(room.ID, "dm:"), ":")
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return parts
	}
	return nil
}

// v1DMName is how one participant reads in a title: an agent's slug (`nina@reviewbot` is "nina"), a person's name.
func v1DMName(id string, agents map[string]v1ImportAgent) string {
	if agent, ok := agents[id]; ok && agent.Name != "" {
		return agent.Name
	}
	if i := strings.Index(id, "@"); i > 0 {
		return id[:i]
	}
	return id
}

// v1DMTitle titles a DM by the participants other than the owner's own accounts: "nina" for the owner and an AI, "nina · wren" for two AIs
// or a mix (the same separator v2 uses for its own private chats). If nobody else remains it falls back to everyone, so the title is never
// empty.
func v1DMTitle(room v1ImportRoom, agents map[string]v1ImportAgent, humans map[string]string, owner string) string {
	var others, everyone []string
	for _, member := range v1DMMembers(room) {
		name := v1DMName(member, agents)
		everyone = append(everyone, name)
		// The owner's own accounts, and a person who is skipped (no live account), are not "the other participant".
		if mapped, isHuman := humans[member], v1ImportKind(member, agents) == "user"; isHuman && (mapped == owner || mapped == "-" || mapped == "") {
			continue
		}
		others = append(others, name)
	}
	if len(others) == 0 {
		others = everyone
	}
	if len(others) == 0 {
		return room.ID
	}
	return strings.Join(others, " · ")
}

// v1DMSuggestedProject is the project an AI participant's `@project` names, when the wizard already proposes one, else the default.
func v1DMSuggestedProject(room v1ImportRoom, projects map[string]V1ImportProject, fallback string) string {
	for _, member := range v1DMMembers(room) {
		if i := strings.Index(member, "@"); i > 0 {
			if _, ok := projects[member[i+1:]]; ok {
				return member[i+1:]
			}
		}
	}
	return fallback
}
