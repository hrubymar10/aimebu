package protocol

// DoctorLive is the answer of GET /api/doctor: what only the running server knows about the machines connected to it, which `aimebu
// doctor --live` cannot read from the database. A machine's capability flags and its launchers' availability arrive in its hello and
// status frames, so they live in the server's memory.
type DoctorLive struct {
	Machines []DoctorMachine `json:"machines"`
}

type DoctorMachine struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Approved        bool   `json:"approved"`
	Connected       bool   `json:"connected"`
	RestartPending  bool   `json:"restart_pending,omitempty"`
	UpgradeRequired bool   `json:"upgrade_required,omitempty"`
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
