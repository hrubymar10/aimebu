package protocol

import (
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

// DoctorLive is the answer of GET /api/doctor: what only the running server knows about the machines connected to it, which `aimebu
// doctor --live` cannot read from the database. A machine's capability flags and its launchers' availability arrive in its hello and
// status frames, so they live in the server's memory.
type DoctorLive struct {
	Machines []DoctorMachine `json:"machines"`
	Database *DoctorDatabase `json:"database,omitempty"`
}

type DoctorDatabase struct {
	IntegrityCheck  string        `json:"integrity_check"`
	SchemaVersion   int           `json:"schema_version"`
	HasV1SchemaMeta bool          `json:"has_v1_schema_meta"`
	Checks          []DoctorCheck `json:"checks"`
}

type DoctorCheck struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Description string `json:"description"`
	Hint        string `json:"hint,omitempty"`
	Scope       string `json:"scope,omitempty"`
}

type DoctorSession struct {
	ID              string `json:"id"`
	Desired         string `json:"desired"`
	Observed        string `json:"observed"`
	Tool            string `json:"tool"`
	Variant         string `json:"variant"`
	Launcher        string `json:"launcher"`
	Generation      int    `json:"generation"`
	Checkout        string `json:"checkout"`
	Remote          bool   `json:"remote"`
	Containerized   bool   `json:"containerized"`
	Room            string `json:"room"`
	LastTurn        string `json:"last_turn"`
	When            string `json:"when"`
	NativeSessionID string `json:"native_session_id"`
}

type DoctorMachine struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Approved          bool   `json:"approved"`
	Connected         bool   `json:"connected"`
	SupervisorVersion string `json:"supervisor_version,omitempty"`
	ServerVersion     string `json:"server_version,omitempty"`
	OlderBuild        bool   `json:"older_build"`
	RestartPending    bool   `json:"restart_pending,omitempty"`
	UpgradeRequired   bool   `json:"upgrade_required,omitempty"`
	// Supports has one entry per capability flag of the hello, in a fixed order; it is empty for a machine that is not connected.
	Supports  []DoctorSupport  `json:"supports,omitempty"`
	Launchers []DoctorLauncher `json:"launchers"`
}

// DoctorSupport is one capability flag and what the server does without it.
type DoctorSupport struct {
	Flag      string `json:"flag"`
	Supported bool   `json:"supported"`
	// Degrades says what stops working, or works worse, on a supervisor without the flag.
	Degrades string `json:"degrades"`
}

type DoctorLauncher struct {
	ID                string   `json:"id"`
	Tool              string   `json:"tool"`
	Name              string   `json:"name"`
	Label             string   `json:"label,omitempty"`
	Enabled           bool     `json:"enabled"`
	Containerized     bool     `json:"containerized"`
	Reported          bool     `json:"reported"` // the connected supervisor has reported this launcher
	Available         bool     `json:"available"`
	UnavailableReason string   `json:"unavailable_reason,omitempty"`
	Version           string   `json:"version,omitempty"`
	Features          []string `json:"features,omitempty"`
	FeatureProblem    string   `json:"feature_problem,omitempty"`
}

// DoctorSelfTest is the answer of GET /api/doctor/self-test: what `aimebu self-test` needs from the running server to run the live eval
// (docs/eval.md) without a config file or a session cookie. Launchers are the rows of the machine the server runs on that the eval can use
// (enabled, Docker, available), as that machine's supervisor last reported them; Targets are every model the owner lists for each of them;
// Usages is the usage snapshot (the shape of GET /api/usages, without any account name) the quota guard reads; null when the server has none yet.
type DoctorSelfTest struct {
	Launchers []LauncherRow          `json:"launchers"`
	Targets   []DoctorSelfTestTarget `json:"targets"`
	Usages    json.RawMessage        `json:"usages"`
	// Skipped are the enabled, available launchers of that machine the eval cannot run through (not Docker), so a person is told why a launcher
	// they have is not in the run.
	Skipped []DoctorSelfTestSkipped `json:"skipped"`
}

// DoctorSelfTestSkipped is one launcher left out of Launchers and Targets, with the reason in words.
type DoctorSelfTestSkipped struct {
	ID     string `json:"id"`
	Tool   string `json:"tool"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// DoctorSelfTestTarget is one model on one launcher. Name is a plain word that is unique in the answer. Providers are the custom providers
// of the launcher (OpenCode); the run sends only the one whose "<id>/" prefix the model has, as a real turn does.
type DoctorSelfTestTarget struct {
	Name       string                   `json:"name"`
	Tool       string                   `json:"tool"`
	LauncherID string                   `json:"launcher_id"`
	Model      string                   `json:"model"`
	Providers  []adapter.CustomProvider `json:"providers,omitempty"`
}
