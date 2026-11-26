package switcher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/goccy/go-json"
)

// ManualChoice records the most recent profile selected by a person. It lives
// beside state.json so a CLI import is visible to a running server and survives
// a restart. An absent file means no manual choice has been recorded.
type ManualChoice struct {
	Profile string    `json:"profile"`
	At      time.Time `json:"at"`
}

func (m *Manager) manualChoicePath(tool string) string {
	return filepath.Join(m.root, "manual-choice-"+tool+".json")
}

func (m *Manager) ManualChoice(tool string) (ManualChoice, error) {
	if err := validateTool(tool); err != nil {
		return ManualChoice{}, err
	}
	data, err := os.ReadFile(m.manualChoicePath(tool))
	if errors.Is(err, os.ErrNotExist) {
		return ManualChoice{}, nil
	}
	if err != nil {
		return ManualChoice{}, fmt.Errorf("read manual choice: %w", err)
	}
	var choice ManualChoice
	if err := json.Unmarshal(data, &choice); err != nil {
		return ManualChoice{}, fmt.Errorf("parse manual choice: %w", err)
	}
	if err := validateProfileName(choice.Profile); err != nil || choice.At.IsZero() {
		return ManualChoice{}, errors.New("invalid manual choice")
	}
	return choice, nil
}

// saveStateWithChoice is called with the switcher lock held. Writing the choice
// first leaves only an ignored marker if the state update later fails.
func (m *Manager) saveStateWithChoice(state State, tool, profile string, automatic bool) error {
	if !automatic {
		if err := m.writeManualChoice(tool, ManualChoice{Profile: profile, At: time.Now().UTC()}); err != nil {
			return err
		}
	}
	return m.saveState(state)
}

func (m *Manager) writeManualChoice(tool string, choice ManualChoice) error {
	data, err := json.Marshal(choice)
	if err != nil {
		return err
	}
	if err := atomicWrite(m.manualChoicePath(tool), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write manual choice: %w", err)
	}
	return nil
}
