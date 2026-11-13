package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/getsentry/sentry-go"
	"github.com/hrubymar10/aimebu/internal/telemetry"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu"
	"github.com/hrubymar10/aimebu/internal/auth"
	"github.com/hrubymar10/aimebu/internal/quota"
	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/switcher"
	"github.com/hrubymar10/aimebu/internal/usages"
	"github.com/hrubymar10/aimebu/internal/usages/providers"
)

type Server struct {
	Store *store.Store
	// NoWebBundle serves the page from its modules instead of the bundle (a test sets it; AIMEBU2_WEB_BUNDLE=off does the same for a whole process).
	NoWebBundle bool
	// legacyAttr caches which Claude Code session a v1 sender's work is in (see resolveLegacyStart), so opening a profile and then starting the
	// agent from it reads the transcripts once.
	legacyAttrMu    sync.Mutex
	legacyAttr      map[string]legacyAttrEntry
	legacyAttrCalls map[string]*legacyAttrCall
	// outputReads is when each person last read an agent's output, for the limit on how often they may (see agentOutput).
	outputMu    sync.Mutex
	outputReads map[string][]time.Time
	// exportPageFn reads one page of a room for the export (a test replaces it to fail partway; nil means the store's own), and exportPageSize
	// is the page size (a test makes it small; 0 means 500).
	exportMu       sync.RWMutex
	exportPageSize int
	exportPageFn   func(ctx context.Context, roomID string, after int64, limit int) ([]store.Message, error)
	Auth           *auth.Service
	Usages         *usages.Reader
	refresherMu    sync.Mutex
	usageRefreshes sync.WaitGroup // the usage refreshes a switch started in the background (see refreshSwitchedUsage)
	refresher      *usages.TargetRefresher
	claimsDone     chan struct{} // closed once the live-file claims are released after shutdown
	liveClaims     *switcher.LiveClaims
	// refresherBlocked is why this process may not refresh a tool's live login: another refresher's claim, or another
	// root's token-copy marker for the same live file. "" means it may.
	refresherBlocked func(tool string) string
	tokenSetting     *usages.TokenSetting
	// quota is read from the usages update hook, which runs under the usage reader's lock, so it is an
	// atomic pointer: taking refresherMu there would invert the order StartTokenCopies uses (refresherMu,
	// then the reader's lock).
	quota atomic.Pointer[quota.Runner]
	// guard is the limit guard, started with the quota switcher and read the same way.
	guard atomic.Pointer[quota.Guard]
	// switcherAfterPreCheck is a test seam: it runs between the web switch's unlocked check and the switch.
	switcherAfterPreCheck func()
	// TokenAutoRefreshAvailable says whether the refresh executor can run; nil means
	// harness-docker-ctrl on PATH. Tests replace it.
	TokenAutoRefreshAvailable func() bool
	// Tests override the settings migration's Codex process scan.
	tokenSettingRunningCodex func() ([]string, string, error)
	// TokenSettingClock is the clock of the setting's waiting backoff; nil is time.Now.
	TokenSettingClock  func() time.Time
	CopilotFlow        *providers.CopilotFlow
	Switcher           *switcher.Manager
	ImportV1Root       string
	DataRoot           string           // the config root; falls back to ImportV1Root
	Version            string           // the build's version string, shown in the page header (GET /api/buildinfo)
	telemetryTransport sentry.Transport // tests replace the network transport; nil is the real one
	CleanupClock       func() time.Time // tests set the clock of the daily cleanup; nil is time.Now
	// claudeModelsFetch gets the Anthropic SDK source Claude's model list is read from; tests replace it. nil is the real fetch.
	claudeModelsFetch func(ctx context.Context, etag string) (body []byte, newETag string, notModified bool, err error)
	claudeModels      claudeModelCache
	checkoutLocks     sync.Map // room ID -> *sync.Mutex: serializes a room's first checkout decision
	diskMu            sync.Mutex
	cleanupAttempt    cleanupAttempts
	diskCache         *diskReport
	TLSPort           int
	// Setup is the first-run owner setup; nil, or closed, once an owner exists.
	Setup                *auth.OwnerSetup
	maintenance          atomic.Bool
	maintenanceMu        sync.RWMutex
	importMu             sync.Mutex
	importReceipt        string
	importJob            importV1JobState
	PollInterval         time.Duration
	WriteTimeout         time.Duration
	QueueSize            int
	MachinePingInterval  time.Duration
	SessionCheckInterval time.Duration
	HeartbeatInterval    time.Duration // how often the browser event stream sends a heartbeat frame (default 20s); the page reconnects when they stop
	EventBatchQuery      func(context.Context, string, int64, int64, int) ([]store.Event, error)
	SessionCheckQuery    func(context.Context, string) error
	threadCountsQuery    func(context.Context, string, []store.Message) (map[string]int, error)
	Allow                []netip.Prefix
	TrustedProxies       []netip.Prefix // AIMEBU2_TRUSTED_PROXIES; see clientIP
	AllowedHosts         []string       // AIMEBU2_ALLOWED_HOSTS: host names, besides localhost, IPs and the public URL's, the routes that need no login answer to
	doctorKey            string         // set by EnableDoctorKey; empty keeps GET /api/doctor closed
	machineMu            sync.Mutex
	kaScopeMu            sync.Mutex                                         // guards kaScope
	kaScope              map[string]keepAliveScopeLease                     // agents with a cache keep-alive ping running: their toolbox is refused (see keep_alive_scope.go)
	warmSeenMu           sync.Mutex                                         // guards warmSeen
	warmSeen             map[string]warmSeenEntry                           // each machine's last count of warm processes, kept after it disconnects (see hostMayHaveWarm)
	saveWarmSeen         func(ctx context.Context, key, value string) error // tests replace the write of a machine's last warm report
	kaSwitchMu           sync.Mutex                                         // guards kaSwitching
	kaSwitching          map[string]int                                     // tools whose account is being switched right now: their cache pings are held back
	machines             map[string]*machineConn
	spawnMu              sync.Mutex
	spawnKeys            map[string]string
	usageWatchMu         sync.Mutex
	usageWatchers        map[chan struct{}]struct{}
	usageAlerts          *usages.AlertTracker
	usageAlertWake       chan struct{}
	telemetryIDs         sync.Map // user id -> the id that person's reports carry (see telemetry_identity.go)
}

func New(db *store.Store) *Server {
	s := &Server{Store: db, Auth: auth.NewService(db), PollInterval: 50 * time.Millisecond, WriteTimeout: 5 * time.Second, QueueSize: 32, MachinePingInterval: 15 * time.Second, SessionCheckInterval: 30 * time.Second,
		Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}, machines: make(map[string]*machineConn), spawnKeys: make(map[string]string), CopilotFlow: providers.NewCopilotFlow()}
	s.Auth.UserHook = s.noteTelemetryUser
	return s
}

func (s *Server) Handler() http.Handler {
	if s.Usages != nil {
		s.Usages.SetUpdateHook(s.usagesChanged)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", s.hostChecked(s.login))
	mux.Handle("GET /api/telemetry/settings", s.Auth.RequireUser(http.HandlerFunc(s.telemetrySettings)))
	mux.Handle("PUT /api/telemetry/settings", s.Auth.RequireUser(http.HandlerFunc(s.telemetrySettings)))
	mux.Handle("GET /api/me/telemetry", s.Auth.RequireUser(http.HandlerFunc(s.telemetryIdentity)))
	mux.Handle("PUT /api/me/telemetry", s.Auth.RequireUser(http.HandlerFunc(s.telemetryIdentity)))
	mux.Handle("POST /api/me/telemetry/fingerprint", s.Auth.RequireUser(http.HandlerFunc(s.telemetryIdentity)))
	mux.HandleFunc("GET /api/setup/status", s.hostChecked(s.setupStatus))
	mux.HandleFunc("POST /api/setup/token", s.hostChecked(s.setupToken))
	mux.HandleFunc("POST /api/setup", s.hostChecked(s.setupOwner))
	mux.HandleFunc("POST /api/invites/redeem", s.hostChecked(s.redeemUserInvite))
	mux.HandleFunc("POST /api/invites/inspect", s.hostChecked(s.inspectUserInvite))
	mux.Handle("POST /api/logout", s.Auth.RequireUser(http.HandlerFunc(s.logout)))
	mux.Handle("GET /api/me", s.Auth.RequireUser(http.HandlerFunc(s.me)))
	mux.Handle("GET /api/buildinfo", s.Auth.RequireUser(http.HandlerFunc(s.buildInfo)))
	mux.Handle("GET /api/me/preferences", s.Auth.RequireUser(http.HandlerFunc(s.preferencesGet)))
	mux.Handle("PUT /api/me/preferences", s.Auth.RequireUser(http.HandlerFunc(s.preferencesPut)))
	mux.Handle("GET /api/disk", s.Auth.RequireUser(http.HandlerFunc(s.diskReport)))
	mux.Handle("POST /api/cleanup", s.Auth.RequireUser(http.HandlerFunc(s.cleanupNow)))
	mux.Handle("GET /api/cleanup/settings", s.Auth.RequireUser(http.HandlerFunc(s.cleanupSettings)))
	mux.Handle("PUT /api/cleanup/settings", s.Auth.RequireUser(http.HandlerFunc(s.cleanupSettings)))
	mux.Handle("GET /api/cleanup/log", s.Auth.RequireUser(http.HandlerFunc(s.cleanupLog)))
	mux.Handle("GET /api/users", s.Auth.RequireUser(http.HandlerFunc(s.users)))
	mux.Handle("POST /api/invites", s.Auth.RequireUser(http.HandlerFunc(s.createUserInvite)))
	mux.Handle("PUT /api/projects/{project}/members/{user}", s.Auth.RequireUser(http.HandlerFunc(s.projectMember)))
	mux.Handle("DELETE /api/projects/{project}/members/{user}", s.Auth.RequireUser(http.HandlerFunc(s.projectMember)))
	mux.Handle("POST /api/users/{user}/deactivate", s.Auth.RequireUser(http.HandlerFunc(s.deactivateUser)))
	mux.Handle("PUT /api/users/{user}/admin", s.Auth.RequireUser(http.HandlerFunc(s.setUserAdmin)))
	mux.Handle("GET /api/import-v1/status", s.Auth.RequireUser(http.HandlerFunc(s.importV1Status)))
	mux.Handle("GET /api/import-v1/preview", s.Auth.RequireUser(http.HandlerFunc(s.importV1Preview)))
	mux.Handle("POST /api/import-v1/dry-run", s.Auth.RequireUser(http.HandlerFunc(s.importV1DryRun)))
	mux.Handle("POST /api/import-v1/commit", s.Auth.RequireUser(http.HandlerFunc(s.importV1Commit)))
	mux.Handle("POST /api/import-v1/skip", s.Auth.RequireUser(http.HandlerFunc(s.importV1Skip)))
	mux.Handle("GET /api/import-v1/jobs/{id}", s.Auth.RequireUser(http.HandlerFunc(s.importV1Job)))
	mux.Handle("GET /api/credentials/targets", s.Auth.RequireUser(http.HandlerFunc(s.credentialTargets)))
	mux.Handle("GET /api/quota-switcher", s.Auth.RequireUser(http.HandlerFunc(s.quotaOverview)))
	mux.Handle("PUT /api/quota-switcher/{tool}", s.Auth.RequireUser(http.HandlerFunc(s.quotaSave)))
	mux.Handle("POST /api/quota-switcher/{tool}/rearm", s.Auth.RequireUser(s.quotaClear(true)))
	mux.Handle("POST /api/quota-switcher/{tool}/reenable", s.Auth.RequireUser(s.quotaClear(false)))
	mux.Handle("GET /api/limit-guard", s.Auth.RequireUser(http.HandlerFunc(s.guardOverview)))
	mux.Handle("PUT /api/limit-guard/{tool}", s.Auth.RequireUser(http.HandlerFunc(s.guardSave)))
	mux.Handle("GET /api/usages", s.Auth.RequireUser(http.HandlerFunc(s.usages)))
	mux.Handle("POST /api/usages/refresh", s.Auth.RequireUser(http.HandlerFunc(s.usageRefresh)))
	mux.Handle("POST /api/usages/providers", s.Auth.RequireUser(http.HandlerFunc(s.usageProvider)))
	mux.Handle("POST /api/usages/settings", s.Auth.RequireUser(http.HandlerFunc(s.usageSettings)))
	mux.Handle("POST /api/usages/mistral/config", s.Auth.RequireUser(http.HandlerFunc(s.usageMistral)))
	mux.Handle("POST /api/usages/ollama/config", s.Auth.RequireUser(http.HandlerFunc(s.usageOllama)))
	mux.Handle("POST /api/usages/copilot/login/start", s.Auth.RequireUser(http.HandlerFunc(s.usageCopilotStart)))
	mux.Handle("POST /api/usages/copilot/login/poll", s.Auth.RequireUser(http.HandlerFunc(s.usageCopilotPoll)))
	mux.Handle("POST /api/usages/copilot/login/logout", s.Auth.RequireUser(http.HandlerFunc(s.usageCopilotLogout)))
	mux.Handle("GET /api/switcher", s.Auth.RequireUser(http.HandlerFunc(s.switcherStatus)))
	mux.Handle("PUT /api/switcher", s.Auth.RequireUser(http.HandlerFunc(s.switcherEnabled)))
	mux.Handle("POST /api/switcher/{tool}/profiles", s.Auth.RequireUser(http.HandlerFunc(s.switcherProfile)))
	mux.Handle("POST /api/switcher/{tool}/import", s.Auth.RequireUser(http.HandlerFunc(s.switcherImport)))
	mux.Handle("POST /api/switcher/{tool}/use", s.Auth.RequireUser(http.HandlerFunc(s.switcherUse)))
	mux.Handle("DELETE /api/switcher/{tool}/profiles/{name}", s.Auth.RequireUser(http.HandlerFunc(s.switcherRemove)))
	mux.Handle("POST /api/switcher/{tool}/profiles/{name}/rename", s.Auth.RequireUser(http.HandlerFunc(s.switcherRename)))
	mux.Handle("GET /api/spawn-catalog", s.Auth.RequireUser(http.HandlerFunc(s.spawnCatalog)))
	mux.Handle("GET /api/sounds", s.Auth.RequireUser(http.HandlerFunc(s.sounds)))
	mux.Handle("POST /api/sounds", s.Auth.RequireUser(http.HandlerFunc(s.uploadSound)))
	mux.Handle("GET /api/sounds/{sound}", s.Auth.RequireUser(http.HandlerFunc(s.downloadSound)))
	mux.Handle("DELETE /api/sounds/{sound}", s.Auth.RequireUser(http.HandlerFunc(s.deleteSound)))
	mux.HandleFunc("GET /api/session", s.sessionProbe)
	mux.Handle("GET /api/machines", s.Auth.RequireUser(http.HandlerFunc(s.machineList)))
	mux.Handle("GET /api/machines/directories", s.Auth.RequireUser(http.HandlerFunc(s.directoryList)))
	mux.Handle("GET /api/machines/{id}/harnesses", s.Auth.RequireUser(http.HandlerFunc(s.machineHarnesses)))
	mux.Handle("PUT /api/machines/{id}/harnesses/{tool}/{variant}", s.Auth.RequireUser(http.HandlerFunc(s.machineHarness)))
	mux.Handle("GET /api/machines/{id}/launchers", s.Auth.RequireUser(http.HandlerFunc(s.machineLaunchers)))
	mux.Handle("POST /api/machines/{id}/launchers", s.Auth.RequireUser(http.HandlerFunc(s.machineLauncher)))
	mux.Handle("PUT /api/machines/{id}/launchers/{launcher}", s.Auth.RequireUser(http.HandlerFunc(s.machineLauncher)))
	mux.Handle("DELETE /api/machines/{id}/launchers/{launcher}", s.Auth.RequireUser(http.HandlerFunc(s.machineLauncher)))
	mux.Handle("PUT /api/machines/{id}/launchers/{launcher}/models", s.Auth.RequireUser(http.HandlerFunc(s.machineHarnessModels)))
	mux.Handle("GET /api/machines/{id}/launchers/{launcher}/providers", s.Auth.RequireUser(http.HandlerFunc(s.machineHarnessProvidersGet)))
	mux.Handle("PUT /api/machines/{id}/launchers/{launcher}/providers", s.Auth.RequireUser(http.HandlerFunc(s.machineHarnessProviders)))
	mux.Handle("GET /api/machines/{id}/launchers/{launcher}/providers/key-status", s.Auth.RequireUser(http.HandlerFunc(s.machineProviderKeys)))
	mux.Handle("POST /api/machines/{id}/launchers/{launcher}/providers/import", s.Auth.RequireUser(http.HandlerFunc(s.machineProviderImport)))
	mux.Handle("POST /api/machines/{id}/launchers/detect", s.Auth.RequireUser(http.HandlerFunc(s.machineHarnessDetect)))
	mux.Handle("POST /api/machines/{id}/launchers/{launcher}/models/detect", s.Auth.RequireUser(http.HandlerFunc(s.machineHarnessModelsDetect)))
	mux.Handle("POST /api/machines/{id}/launchers/{launcher}/models/sync", s.Auth.RequireUser(http.HandlerFunc(s.machineHarnessModelsSync)))
	mux.Handle("POST /api/machine-pairings", s.Auth.RequireUser(http.HandlerFunc(s.machinePairing)))
	mux.HandleFunc("POST /api/machine-pairings/redeem", s.hostChecked(s.redeemMachinePairing))
	mux.HandleFunc("GET /api/machines/{id}/pair-status", s.machinePairStatus)
	mux.Handle("POST /api/machines/{id}/agents/stop", s.Auth.RequireUser(http.HandlerFunc(s.machineStopAgents)))
	mux.Handle("POST /api/machines/{id}/approve", s.Auth.RequireUser(http.HandlerFunc(s.machineApproval)))
	mux.Handle("POST /api/machines/{id}/revoke", s.Auth.RequireUser(http.HandlerFunc(s.machineRevocation)))
	mux.Handle("GET /api/projects/{project}/machine-folders", s.Auth.RequireUser(http.HandlerFunc(s.machineFolders)))
	mux.Handle("PUT /api/projects/{project}/machine-folders/{machine}", s.Auth.RequireUser(http.HandlerFunc(s.machineFolder)))
	mux.Handle("POST /api/projects/{project}/machine-folders/{machine}/admit", s.Auth.RequireUser(http.HandlerFunc(s.machineAdmit)))
	mux.Handle("GET /api/roles", s.Auth.RequireUser(http.HandlerFunc(s.roles)))
	mux.Handle("POST /api/roles", s.Auth.RequireUser(http.HandlerFunc(s.roles)))
	mux.Handle("PUT /api/roles/{id}", s.Auth.RequireUser(http.HandlerFunc(s.role)))
	mux.Handle("DELETE /api/roles/{id}", s.Auth.RequireUser(http.HandlerFunc(s.role)))
	mux.Handle("POST /api/roles/{id}/reset", s.Auth.RequireUser(http.HandlerFunc(s.roleReset)))
	mux.Handle("GET /api/projects/{project}/role-spawn-defaults", s.Auth.RequireUser(http.HandlerFunc(s.projectRoleSpawnDefaults)))
	mux.Handle("PUT /api/projects/{project}/role-spawn-defaults/{role}", s.Auth.RequireUser(http.HandlerFunc(s.projectRoleSpawnDefault)))
	mux.Handle("GET /api/projects/{project}/role-spawn-defaults/{role}/next", s.Auth.RequireUser(http.HandlerFunc(s.projectRoleSpawnDefaultNext)))
	mux.Handle("GET /api/agent-isolation", s.Auth.RequireUser(http.HandlerFunc(s.agentIsolation)))
	mux.Handle("PUT /api/agent-isolation", s.Auth.RequireUser(http.HandlerFunc(s.agentIsolation)))
	mux.Handle("GET /api/retention", s.Auth.RequireUser(http.HandlerFunc(s.retention)))
	mux.Handle("PUT /api/retention", s.Auth.RequireUser(http.HandlerFunc(s.retention)))
	mux.Handle("GET /api/prompts", s.Auth.RequireUser(http.HandlerFunc(s.prompts)))
	mux.Handle("PUT /api/prompts/{key}", s.Auth.RequireUser(http.HandlerFunc(s.promptOverride)))
	mux.Handle("DELETE /api/prompts/{key}", s.Auth.RequireUser(http.HandlerFunc(s.promptOverride)))
	mux.Handle("PUT /api/inline-plans", s.Auth.RequireUser(http.HandlerFunc(s.inlinePlans)))
	mux.Handle("POST /api/welcome/seen", s.Auth.RequireUser(http.HandlerFunc(s.welcomeSeen)))
	mux.Handle("GET /api/macros", s.Auth.RequireUser(http.HandlerFunc(s.macros)))
	mux.Handle("GET /api/settings-backup", s.Auth.RequireUser(http.HandlerFunc(s.settingsBackupExport)))
	mux.Handle("POST /api/settings-backup/preview", s.Auth.RequireUser(http.HandlerFunc(s.settingsBackupPreview)))
	mux.Handle("POST /api/settings-backup/apply", s.Auth.RequireUser(http.HandlerFunc(s.settingsBackupApply)))
	mux.Handle("PUT /api/macros", s.Auth.RequireUser(http.HandlerFunc(s.macros)))
	mux.Handle("GET /api/projects", s.Auth.RequireUser(http.HandlerFunc(s.projects)))
	mux.Handle("GET /api/health", s.Auth.RequireUser(http.HandlerFunc(s.health)))
	mux.Handle("POST /api/projects", s.Auth.RequireUser(http.HandlerFunc(s.projects)))
	mux.Handle("PATCH /api/projects/{project}", s.Auth.RequireUser(http.HandlerFunc(s.projectUpdate)))
	mux.Handle("POST /api/projects/{project}/archive", s.Auth.RequireUser(http.HandlerFunc(s.archiveProject)))
	mux.Handle("POST /api/projects/{project}/reopen", s.Auth.RequireUser(http.HandlerFunc(s.reopenProject)))
	mux.Handle("DELETE /api/projects/{project}", s.Auth.RequireUser(http.HandlerFunc(s.deleteProject)))
	mux.Handle("GET /api/projects/{project}/settings", s.Auth.RequireUser(http.HandlerFunc(s.projectRecoverySettings)))
	mux.Handle("PUT /api/projects/{project}/settings", s.Auth.RequireUser(http.HandlerFunc(s.projectRecoverySettings)))
	mux.Handle("GET /api/projects/{project}/memory", s.Auth.RequireUser(http.HandlerFunc(s.projectMemory)))
	mux.Handle("POST /api/projects/{project}/memory", s.Auth.RequireUser(http.HandlerFunc(s.projectMemory)))
	mux.Handle("PUT /api/projects/{project}/memory-enabled", s.Auth.RequireUser(http.HandlerFunc(s.projectMemoryEnabled)))
	mux.Handle("PUT /api/projects/{project}/memory/{id}", s.Auth.RequireUser(http.HandlerFunc(s.projectMemoryRecord)))
	mux.Handle("DELETE /api/projects/{project}/memory/{id}", s.Auth.RequireUser(http.HandlerFunc(s.projectMemoryRecord)))
	mux.Handle("GET /api/global-memory/{scope}", s.Auth.RequireUser(http.HandlerFunc(s.globalMemory)))
	mux.Handle("POST /api/global-memory/{scope}", s.Auth.RequireUser(http.HandlerFunc(s.globalMemory)))
	mux.Handle("PUT /api/global-memory/{scope}/{id}", s.Auth.RequireUser(http.HandlerFunc(s.globalMemoryRecord)))
	mux.Handle("DELETE /api/global-memory/{scope}/{id}", s.Auth.RequireUser(http.HandlerFunc(s.globalMemoryRecord)))
	mux.Handle("PUT /api/global-memory/{scope}/{id}/activation", s.Auth.RequireUser(http.HandlerFunc(s.globalMemoryActivation)))
	mux.Handle("GET /api/projects/{project}/rooms", s.Auth.RequireUser(http.HandlerFunc(s.rooms)))
	mux.Handle("GET /api/projects/{project}/claude-sessions", s.Auth.RequireUser(http.HandlerFunc(s.claudeSessions)))
	mux.Handle("GET /api/projects/{project}/dm-peers", s.Auth.RequireUser(http.HandlerFunc(s.dmPeers)))
	mux.Handle("POST /api/projects/{project}/dms", s.Auth.RequireUser(http.HandlerFunc(s.openUserDM)))
	mux.Handle("GET /api/projects/{project}/archive/search", s.Auth.RequireUser(http.HandlerFunc(s.archiveSearch)))
	mux.Handle("POST /api/projects/{project}/rooms", s.Auth.RequireUser(http.HandlerFunc(s.rooms)))
	mux.Handle("GET /api/rooms/{room}", s.Auth.RequireUser(http.HandlerFunc(s.room)))
	mux.Handle("PATCH /api/rooms/{room}", s.Auth.RequireUser(http.HandlerFunc(s.roomRename)))
	mux.Handle("PATCH /api/rooms/{room}/preferences", s.Auth.RequireUser(http.HandlerFunc(s.roomPreferences)))
	mux.Handle("POST /api/rooms/{room}/read", s.Auth.RequireUser(http.HandlerFunc(s.roomRead)))
	mux.Handle("GET /api/rooms/{room}/read-receipts", s.Auth.RequireUser(http.HandlerFunc(s.roomReadReceipts)))
	mux.Handle("PUT /api/rooms/{room}/memory-enabled", s.Auth.RequireUser(http.HandlerFunc(s.roomMemoryEnabled)))
	mux.Handle("POST /api/rooms/{room}/close", s.Auth.RequireUser(http.HandlerFunc(s.closeRoom)))
	mux.Handle("DELETE /api/rooms/{room}", s.Auth.RequireUser(http.HandlerFunc(s.removeRoom)))
	mux.Handle("POST /api/rooms/{room}/reopen", s.Auth.RequireUser(http.HandlerFunc(s.reopenRoom)))
	mux.Handle("GET /api/rooms/{room}/worktree", s.Auth.RequireUser(http.HandlerFunc(s.roomWorktree)))
	mux.Handle("POST /api/rooms/{room}/worktree", s.Auth.RequireUser(http.HandlerFunc(s.roomWorktree)))
	mux.Handle("DELETE /api/rooms/{room}/worktree", s.Auth.RequireUser(http.HandlerFunc(s.roomWorktree)))
	mux.Handle("GET /api/rooms/{room}/messages", s.Auth.RequireUser(http.HandlerFunc(s.messages)))
	mux.Handle("GET /api/rooms/{room}/export", s.Auth.RequireUser(http.HandlerFunc(s.exportRoom)))
	mux.Handle("GET /api/rooms/{room}/usage", s.Auth.RequireUser(http.HandlerFunc(s.roomUsage)))
	mux.Handle("GET /api/projects/{project}/usage", s.Auth.RequireUser(http.HandlerFunc(s.projectUsage)))
	mux.Handle("GET /api/rooms/{room}/schedules", s.Auth.RequireUser(http.HandlerFunc(s.schedules)))
	mux.Handle("POST /api/rooms/{room}/schedules", s.Auth.RequireUser(http.HandlerFunc(s.schedules)))
	mux.Handle("PATCH /api/rooms/{room}/schedules/{id}", s.Auth.RequireUser(http.HandlerFunc(s.schedule)))
	mux.Handle("DELETE /api/rooms/{room}/schedules/{id}", s.Auth.RequireUser(http.HandlerFunc(s.schedule)))
	mux.Handle("POST /api/rooms/{room}/messages", s.Auth.RequireUser(http.HandlerFunc(s.messages)))
	mux.Handle("POST /api/rooms/{room}/attachments", s.Auth.RequireUser(http.HandlerFunc(s.uploadAttachment)))
	mux.Handle("DELETE /api/rooms/{room}/attachments/{attachment}", s.Auth.RequireUser(http.HandlerFunc(s.deleteAttachment)))
	mux.Handle("GET /api/attachments/{attachment}", s.Auth.RequireUser(http.HandlerFunc(s.downloadAttachment)))
	mux.HandleFunc("GET /api/agent/turns/{turn}/attachments/{attachment}", s.turnAttachment)
	mux.Handle("GET /api/rooms/{room}/task-notes", s.Auth.RequireUser(http.HandlerFunc(s.taskNotes)))
	mux.Handle("POST /api/rooms/{room}/task-notes", s.Auth.RequireUser(http.HandlerFunc(s.taskNotes)))
	mux.Handle("PUT /api/rooms/{room}/task-notes/{id}", s.Auth.RequireUser(http.HandlerFunc(s.taskNote)))
	mux.Handle("DELETE /api/rooms/{room}/task-notes/{id}", s.Auth.RequireUser(http.HandlerFunc(s.taskNote)))
	mux.Handle("GET /api/rooms/{room}/mention-users", s.Auth.RequireUser(http.HandlerFunc(s.roomMentionUsers)))
	mux.Handle("GET /api/rooms/{room}/members", s.Auth.RequireUser(http.HandlerFunc(s.roomMembers)))
	mux.Handle("GET /api/rooms/{room}/agents", s.Auth.RequireUser(http.HandlerFunc(s.roomAgents)))
	mux.Handle("GET /api/rooms/{room}/agent-name", s.Auth.RequireUser(http.HandlerFunc(s.agentNameSuggestion)))
	mux.Handle("GET /api/rooms/{room}/agents/{agent}/profile", s.Auth.RequireUser(http.HandlerFunc(s.agentProfile)))
	mux.Handle("GET /api/rooms/{room}/agents/{agent}/output", s.Auth.RequireUser(http.HandlerFunc(s.agentOutput)))
	mux.Handle("POST /api/rooms/{room}/agents", s.Auth.RequireUser(http.HandlerFunc(s.roomAgents)))
	mux.Handle("GET /api/rooms/{room}/control", s.Auth.RequireUser(http.HandlerFunc(s.scopeControl)))
	mux.Handle("POST /api/rooms/{room}/control", s.Auth.RequireUser(http.HandlerFunc(s.scopeControl)))
	mux.Handle("GET /api/projects/{project}/control", s.Auth.RequireUser(http.HandlerFunc(s.scopeControl)))
	mux.Handle("POST /api/projects/{project}/control", s.Auth.RequireUser(http.HandlerFunc(s.scopeControl)))
	mux.Handle("GET /api/usage-limit-default", s.Auth.RequireUser(http.HandlerFunc(s.usageLimitDefault)))
	mux.Handle("PUT /api/usage-limit-default", s.Auth.RequireUser(http.HandlerFunc(s.usageLimitDefault)))
	mux.Handle("GET /api/projects/{project}/usage-limit", s.Auth.RequireUser(http.HandlerFunc(s.usageLimit)))
	mux.Handle("PUT /api/projects/{project}/usage-limit", s.Auth.RequireUser(http.HandlerFunc(s.usageLimit)))
	mux.Handle("POST /api/agents/{id}/control", s.Auth.RequireUser(http.HandlerFunc(s.agentControl)))
	mux.Handle("DELETE /api/agents/{id}/worktree", s.Auth.RequireUser(http.HandlerFunc(s.agentWorktreeRemove)))
	mux.Handle("DELETE /api/agents/{id}", s.Auth.RequireUser(http.HandlerFunc(s.agentRemove)))
	mux.Handle("POST /api/agents/{id}/dm", s.Auth.RequireUser(http.HandlerFunc(s.openAgentDM)))
	mux.Handle("POST /api/agents/{id}/switch", s.Auth.RequireUser(http.HandlerFunc(s.agentSwitch)))
	mux.Handle("POST /api/inbox/{item}/answer", s.Auth.RequireUser(http.HandlerFunc(s.answerInbox)))
	mux.Handle("POST /api/inbox/{item}/decision", s.Auth.RequireUser(http.HandlerFunc(s.decideRecovery)))
	mux.Handle("POST /api/inbox/{item}/dismiss", s.Auth.RequireUser(http.HandlerFunc(s.dismissDeliveryFailure)))
	mux.Handle("GET /api/system-events", s.Auth.RequireUser(http.HandlerFunc(s.systemEvents)))
	mux.Handle("GET /api/message-numbers/{no}", s.Auth.RequireUser(http.HandlerFunc(s.messageByNo)))
	mux.Handle("GET /api/messages/{message}/debug", s.Auth.RequireUser(http.HandlerFunc(s.messageDebug)))
	mux.Handle("GET /api/messages/{message}/reactions", s.Auth.RequireUser(http.HandlerFunc(s.reactions)))
	mux.Handle("PUT /api/messages/{message}/reactions/{emoji}", s.Auth.RequireUser(http.HandlerFunc(s.reactions)))
	mux.Handle("DELETE /api/messages/{message}/reactions/{emoji}", s.Auth.RequireUser(http.HandlerFunc(s.reactions)))
	mux.Handle("GET /api/events", s.Auth.RequireUser(http.HandlerFunc(s.events)))
	mux.Handle("GET /api/events/cursor", s.Auth.RequireUser(http.HandlerFunc(s.eventCursor)))
	mux.HandleFunc("GET /api/machine", s.machineSocket)
	mux.HandleFunc("GET /api/doctor", s.doctorLive)
	mux.Handle("GET /api/inbox", s.Auth.RequireUser(http.HandlerFunc(s.inbox)))
	mux.Handle("GET /api/agent/rooms", s.requireAgentTool(http.HandlerFunc(s.agentToolRooms)))
	mux.Handle("GET /api/agent/agents", s.requireAgentTool(http.HandlerFunc(s.agentToolAgents)))
	mux.Handle("GET /api/agent/rooms/{room}/history", s.requireAgentTool(http.HandlerFunc(s.agentToolRoomHistory)))
	mux.Handle("GET /api/agent/macros", s.requireAgentTool(http.HandlerFunc(s.agentToolMacros)))
	mux.Handle("GET /api/agent/rooms/{room}/schedules", s.requireAgentTool(http.HandlerFunc(s.agentToolSchedules)))
	mux.Handle("POST /api/agent/rooms/{room}/schedules", s.requireAgentTool(http.HandlerFunc(s.agentToolSchedules)))
	mux.Handle("POST /api/agent/dms", s.requireAgentTool(http.HandlerFunc(s.agentToolDM)))
	mux.Handle("POST /api/agent/human-dms", s.requireAgentTool(http.HandlerFunc(s.agentToolDMHuman)))
	mux.Handle("GET /api/agent-dms", s.Auth.RequireUser(http.HandlerFunc(s.agentDMs)))
	mux.Handle("POST /api/rooms/{room}/join", s.Auth.RequireUser(http.HandlerFunc(s.joinAgentDM)))
	mux.Handle("POST /api/rooms/{room}/leave", s.Auth.RequireUser(http.HandlerFunc(s.leaveAgentDM)))
	mux.Handle("POST /api/agent/rooms", s.requireAgentTool(http.HandlerFunc(s.agentToolCreateRoom)))
	mux.Handle("POST /api/agent/rooms/{room}/close", s.requireAgentTool(http.HandlerFunc(s.agentToolCloseRoom)))
	mux.Handle("POST /api/agent/mcp", s.requireAgentTool(http.HandlerFunc(s.agentToolMCP)))
	mux.Handle("GET /api/agent/attachments/{attachment}", s.requireAgentTool(http.HandlerFunc(s.agentAttachment)))
	mux.Handle("GET /api/agent/usage-left", s.requireAgentTool(http.HandlerFunc(s.agentToolUsageLeft)))
	mux.Handle("POST /api/agent/memory", s.requireAgentTool(http.HandlerFunc(s.agentToolMemoryAdd)))
	mux.Handle("GET /api/agent/memory", s.requireAgentTool(http.HandlerFunc(s.agentToolMemoryList)))
	mux.Handle("PUT /api/agent/memory/{id}", s.requireAgentTool(http.HandlerFunc(s.agentToolMemoryRecord)))
	mux.Handle("DELETE /api/agent/memory/{id}", s.requireAgentTool(http.HandlerFunc(s.agentToolMemoryRecord)))
	mux.Handle("GET /api/agent/message-numbers/{no}", s.requireAgentTool(http.HandlerFunc(s.agentToolMessageByNo)))
	mux.Handle("GET /api/agent/recall", s.requireAgentTool(http.HandlerFunc(s.agentToolRecall)))
	mux.Handle("POST /api/agent/agents", s.requireAgentTool(http.HandlerFunc(s.agentToolSpawn)))
	mux.Handle("POST /api/agent/agents/{id}/stop", s.requireAgentTool(http.HandlerFunc(s.agentToolStop)))
	mux.Handle("POST /api/agent/agents/{id}/role", s.requireAgentTool(http.HandlerFunc(s.agentToolSetRole)))
	mux.Handle("PUT /api/agent/messages/{message}/reactions/{emoji}", s.requireAgentTool(http.HandlerFunc(s.agentToolReact)))
	mux.Handle("DELETE /api/agent/messages/{message}/reactions/{emoji}", s.requireAgentTool(http.HandlerFunc(s.agentToolReact)))
	assets := aimebu.Web()
	web, err := prepareWebAssets(assets, !s.NoWebBundle && webBundleEnabled())
	if err != nil {
		panic(err)
	}
	files := http.FileServerFS(assets)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "setup" && !s.Setup.Open() {
			http.NotFound(w, r)
			return
		}
		if name == "" || name == "index.html" {
			s.serveAppPage(w, r, web)
			return
		}
		if web.bundle != nil && (name == bundleURL || name == bundleURL+".map") {
			serveWebBundle(w, r, web, name)
			return
		}
		if _, err := fs.Stat(assets, name); err == nil {
			if r.URL.Query().Get("v") == web.version {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			if script, ok := web.scripts[name]; ok {
				w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
				http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(script))
				return
			}
			if strings.HasSuffix(name, ".webmanifest") {
				w.Header().Set("Content-Type", "application/manifest+json")
			}
			files.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.Contains(name, ".") {
			http.NotFound(w, r)
			return
		}
		s.serveAppPage(w, r, web)
	})
	return telemetry.Middleware(allowMiddleware(s.withHSTS(s.maintenanceMiddleware(s.systemProjectGuard(s.archivedGuard(mux)))), s.Allow))
}

// serveWebBundle answers for the bundle (the page asks for it by a name that carries the version, so it is cached for good) or its source map, packed
// with gzip for a browser that accepts it: the bundle is built once, so it is packed once. The map is only asked for by a browser's developer tools.
func serveWebBundle(w http.ResponseWriter, r *http.Request, web webAssets, name string) {
	h := w.Header()
	if r.URL.Query().Get("v") == web.version {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	h.Add("Vary", "Accept-Encoding")
	// An ETag lets a request that is not under the version (and so is revalidated) be answered 304; the packed file is another body and has its own.
	body, kind, tag := web.bundle.js, "text/javascript; charset=utf-8", `"`+web.version+`"`
	if strings.HasSuffix(name, ".map") {
		body, kind, tag = web.bundle.smap, "application/json", `"`+web.version+`-map"`
	} else if acceptsGzip(r.Header.Get("Accept-Encoding")) {
		body, tag = web.bundle.jsGzip, `"`+web.version+`-gzip"`
		h.Set("Content-Encoding", "gzip")
	}
	h.Set("ETag", tag)
	h.Set("Content-Type", kind)
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
}

// acceptsGzip reads an Accept-Encoding header: gzip is accepted when it is listed with a quality above 0; "gzip;q=0" is a refusal even beside "*", and
// "*" alone accepts it.
func acceptsGzip(header string) bool {
	star := false
	for _, part := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "gzip" && name != "*" {
			continue
		}
		accepted := true
		if q, ok := strings.CutPrefix(strings.ReplaceAll(strings.ToLower(params), " ", ""), "q="); ok {
			if value, err := strconv.ParseFloat(q, 64); err == nil && value <= 0 {
				accepted = false
			}
		}
		if name == "gzip" {
			return accepted
		}
		star = accepted
	}
	return star
}

// serveAppPage answers with the app's one HTML page and the headers that keep it to this server: a Content-Security-Policy (see contentSecurityPolicy),
// no framing, no sniffing and no referrer.
func (s *Server) serveAppPage(w http.ResponseWriter, r *http.Request, web webAssets) {
	h := w.Header()
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Type", "text/html; charset=utf-8")
	// The page's own origin for the event socket is named from the Host header only when this server answers to that host: a made-up Host
	// would otherwise write itself into the policy (it could only change the policy of its own response, but there is no reason to let it).
	host := ""
	if s.hostAllowed(r) {
		host = r.Host
	}
	h.Set("Content-Security-Policy", web.contentSecurityPolicy(host, s.cspConnectExtra()))
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(web.index))
}

// cspConnectExtra lists the origins besides this server that the page may send requests to: the error-report ingest origin (the scheme and host
// of the DSN the page is told to report to), and only while reporting is on, so a page of a server that does not report may talk to nobody else.
func (s *Server) cspConnectExtra() []string {
	browser := telemetry.Current().BrowserConfig()
	if browser == nil {
		return nil
	}
	dsn, _ := browser["dsn"].(string)
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil
	}
	return []string{parsed.Scheme + "://" + parsed.Host}
}

func (s *Server) maintenanceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || strings.HasPrefix(r.URL.Path, "/api/import-v1/") {
			next.ServeHTTP(w, r)
			return
		}
		if s.maintenance.Load() {
			w.Header().Set("Retry-After", "5")
			apiError(w, 503, "maintenance", "import in progress; retry this write")
			return
		}
		s.maintenanceMu.RLock()
		defer s.maintenanceMu.RUnlock()
		if s.maintenance.Load() {
			w.Header().Set("Retry-After", "5")
			apiError(w, 503, "maintenance", "import in progress; retry this write")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// usages answers GET /api/usages: the owner reads the whole snapshot, an admin reads the limits without anything that names an account or how it signs in
// (usages.Response.ForAdmin). Every write stays the owner's.
func (s *Server) usages(w http.ResponseWriter, r *http.Request) {
	if !s.adminMachine(w, r) {
		return
	}
	owner, err := s.Store.IsGlobalOwner(r.Context(), userID(r))
	if err != nil {
		fail(w, err)
		return
	}
	if s.Usages == nil {
		apiError(w, 503, "unavailable", "usage snapshots unavailable")
		return
	}
	response := s.Usages.Snapshot()
	if s.Switcher != nil {
		if err := s.Switcher.OverlayUsages(&response); err != nil {
			fail(w, err)
			return
		}
	}
	if !owner {
		response = response.ForAdmin()
	}
	writeJSON(w, 200, response)
}

func readJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	return readJSONLimit(w, r, out, 64<<10)
}

func readJSONLimit(w http.ResponseWriter, r *http.Request, out any, limit int64) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		apiError(w, 415, "invalid_request", "content type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		apiError(w, 400, "invalid_request", "invalid JSON")
		return false
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		apiError(w, 400, "invalid_request", "trailing JSON")
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
func apiError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "message": message})
}
func projectView(p store.Project) map[string]any {
	var repo any
	if p.RepoURL != "" {
		repo = p.RepoURL
	}
	view := map[string]any{"id": p.ID, "name": p.Name, "folder": p.Folder, "repo_url": repo}
	if p.Role != "" {
		view["role"] = p.Role
	}
	if p.ArchivedAt != "" {
		view["archived_at"] = p.ArchivedAt
	}
	return view
}
func messageView(m store.Message) map[string]any {
	targets := []string{}
	_ = json.Unmarshal([]byte(m.TargetsJSON), &targets)
	view := map[string]any{"id": m.ID, "seq": m.Seq, "no": m.No, "room_id": m.RoomID, "author_kind": m.AuthorKind, "author_id": m.AuthorID, "body": m.Body, "targets": targets, "created_at": m.CreatedAt}
	if m.ReplyTo != "" {
		view["reply_to"] = m.ReplyTo
	}
	if m.ThreadRoot != "" {
		view["thread_root"] = m.ThreadRoot
	}
	if m.AskJSON != "" {
		view["ask"] = json.RawMessage(m.AskJSON)
	}
	if m.NeedsAttention {
		view["needs_attention"] = true
	}
	if m.ProposedAnswersJSON != "" {
		view["proposed_answers"] = json.RawMessage(m.ProposedAnswersJSON)
	}
	if m.OpenQuestionsJSON != "" {
		view["open_questions"] = json.RawMessage(m.OpenQuestionsJSON)
	}
	if m.AnswerState != "" {
		view["answer_state"] = m.AnswerState
		if m.AnswerState == "resolved" {
			view["answer_body"] = m.AnswerBody
		}
	}
	if m.VisualPlanJSON != "" {
		view["visual_plan"] = json.RawMessage(m.VisualPlanJSON)
	}
	if m.AppendixPagesJSON != "" {
		view["appendix_pages"] = json.RawMessage(m.AppendixPagesJSON)
	}
	if m.SourceOutID != "" {
		view["source_out_id"] = m.SourceOutID
	}
	if m.AuthorHarness != "" {
		view["author_harness"] = m.AuthorHarness
	}
	if m.AuthorModel != "" {
		view["author_model"] = m.AuthorModel
	}
	if m.AuthorName != "" {
		view["author_name"] = m.AuthorName
	}
	if m.AuthorProjectID != "" {
		view["author_project_id"] = m.AuthorProjectID
	}
	if len(m.Attachments) > 0 {
		view["attachments"] = m.Attachments
	}
	return view
}
func userID(r *http.Request) string { id, _ := auth.UserIDFromContext(r.Context()); return id }
func fail(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		apiError(w, 404, "not_found", "not found")
	} else if errors.Is(err, store.ErrForbidden) {
		apiError(w, 403, "forbidden", "forbidden")
	} else if errors.Is(err, store.ErrScopeMismatch) {
		apiError(w, 409, "scope_mismatch", "the requested resources belong to different projects or machines")
	} else {
		log.Printf("http 500: %v", err)
		if reporter, ok := w.(interface{ CaptureHTTPError(error) }); ok {
			reporter.CaptureHTTPError(err)
		} else {
			telemetry.Current().CaptureError(context.Background(), err)
		}
		apiError(w, 500, "internal_error", "internal server error")
	}
}
func (s *Server) projectAccess(w http.ResponseWriter, r *http.Request, id string) bool {
	if err := s.Store.ProjectAccess(r.Context(), userID(r), id); err != nil {
		fail(w, err)
		return false
	}
	return true
}
func (s *Server) projectOperator(w http.ResponseWriter, r *http.Request, id string) bool {
	role, err := s.Store.UserProjectRole(r.Context(), userID(r), id)
	if err != nil {
		userError(w, err)
		return false
	}
	if role == "viewer" {
		apiError(w, 403, "forbidden", "project owner or operator required")
		return false
	}
	return true
}
func (s *Server) roomAccess(w http.ResponseWriter, r *http.Request, id string) (store.Room, bool) {
	room, err := s.Store.RoomByID(r.Context(), id)
	if err != nil {
		fail(w, err)
		return room, false
	}
	if err := s.Store.UserRoomAccess(r.Context(), userID(r), id); err != nil {
		fail(w, err)
		return room, false
	}
	return room, true
}

func (s *Server) openAgentDM(w http.ResponseWriter, r *http.Request) {
	card, err := s.Store.AgentCardByID(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	if !s.projectOperator(w, r, card.ProjectID) {
		return
	}
	room, err := s.Store.OpenDM(r.Context(), userID(r), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Login    string `json:"login"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	host := s.clientIP(r)
	result, err := s.Auth.Login(r.Context(), req.Login, req.Password, host)
	if err != nil {
		if errors.Is(err, auth.ErrRateLimited) {
			delay := s.Auth.LoginRetryAfter(req.Login, host)
			seconds := int((delay + time.Second - 1) / time.Second)
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			apiError(w, 429, "rate_limited", "rate limited")
		} else if errors.Is(err, auth.ErrInvalidCredentials) {
			apiError(w, 401, "invalid_credentials", "invalid credentials")
		} else {
			fail(w, err)
		}
		return
	}
	http.SetCookie(w, s.Auth.SessionCookie(result.SessionToken, s.Auth.SecureRequest(r)))
	firstRun, err := s.firstRun(r.Context(), result.UserID)
	if err != nil {
		fail(w, err)
		return
	}
	view, err := s.sessionView(r.Context(), result.UserID, map[string]any{"user_id": result.UserID, "csrf_token": result.CSRFToken, "expires_at": result.ExpiresAt, "first_run": firstRun})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, view)
}

// GET /api/setup/status: while a setup token waits, where the person finds it; once an owner exists, just {open:false}. The path is the file the server wrote, not the token.
func (s *Server) setupStatus(w http.ResponseWriter, r *http.Request) {
	if !s.Setup.Open() {
		// Answered, not 404: the page asks on every load and a 404 shows up as a console error.
		writeJSON(w, 200, map[string]any{"open": false})
		return
	}
	writeJSON(w, 200, map[string]any{"open": true, "token_path": s.setupPathFor(r)})
}

// setupPathFor is the path the screen shows. A client on the same machine gets the full path; a remote one only the file's place in
// the config directory, so the page does not hand a stranger the server's user name or directory layout. "On the same machine" means
// the connection itself comes from a loopback address AND carries no forwarding header: a reverse proxy on this host (the Caddy of
// docs/tls.md) also connects from loopback, and what it relays may be anyone.
func (s *Server) setupPathFor(r *http.Request) string {
	full := s.Setup.Path()
	if peer, err := netip.ParseAddrPort(r.RemoteAddr); err == nil && peer.Addr().Unmap().IsLoopback() && !viaProxy(r) {
		return full
	}
	return "<config dir>/" + filepath.Base(full)
}

// viaProxy reports whether the request says it was relayed (so its connection's address is the proxy's, not the person's).
func viaProxy(r *http.Request) bool {
	for _, name := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip", "Forwarded", "Via"} {
		if len(r.Header.Values(name)) > 0 {
			return true
		}
	}
	return false
}

// setupFailure answers the errors the token checks share; it reports whether err was one of them.
func (s *Server) setupFailure(w http.ResponseWriter, err error, host string) bool {
	switch {
	case errors.Is(err, auth.ErrRateLimited):
		seconds := int((s.Setup.RetryAfter(host) + time.Second - 1) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		writeJSON(w, 429, map[string]any{"error": "rate_limited", "message": "too many attempts", "retry_after_sec": seconds})
	case errors.Is(err, auth.ErrSetupClosed):
		apiError(w, 404, "not_found", "not found")
	case errors.Is(err, auth.ErrSetupToken):
		apiError(w, 403, "invalid_setup_token", "that is not the setup token; copy it from the server's output or from the token file it names")
	case errors.Is(err, auth.ErrSetupCredentials):
		apiError(w, 400, "invalid_owner", "choose a login of 3-32 lowercase letters, digits, - or _ starting with a letter, and a password of at least 12 characters")
	default:
		return false
	}
	return true
}

// POST /api/setup/token: says whether a setup token is the right one, so the screen can ask for the owner's login only after that.
// It creates nothing; a wrong token counts against the caller's IP like it does for the setup itself.
func (s *Server) setupToken(w http.ResponseWriter, r *http.Request) {
	if !s.Setup.Open() {
		apiError(w, 404, "not_found", "not found")
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	host := s.clientIP(r)
	if err := s.Setup.Verify(req.Token, host); err != nil {
		if !s.setupFailure(w, err, host) {
			fail(w, err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/setup: the first-run owner setup. The one-time token from the startup message authorizes it; the owner and the
// host machine are created by the same code as the environment bootstrap, and the new owner is signed in.
func (s *Server) setupOwner(w http.ResponseWriter, r *http.Request) {
	if !s.Setup.Open() {
		apiError(w, 404, "not_found", "not found")
		return
	}
	var req struct {
		Token    string `json:"token"`
		Login    string `json:"login"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	host := s.clientIP(r)
	ownerID, err := s.Setup.Redeem(r.Context(), req.Token, req.Login, req.Password, host)
	if err != nil {
		if !s.setupFailure(w, err, host) {
			fail(w, err)
		}
		return
	}
	result, err := s.Auth.StartSession(r.Context(), ownerID)
	if err != nil {
		fail(w, err)
		return
	}
	http.SetCookie(w, s.Auth.SessionCookie(result.SessionToken, s.Auth.SecureRequest(r)))
	firstRun, err := s.firstRun(r.Context(), ownerID)
	if err != nil {
		fail(w, err)
		return
	}
	view, err := s.sessionView(r.Context(), ownerID, map[string]any{"user_id": ownerID, "csrf_token": result.CSRFToken, "expires_at": result.ExpiresAt, "first_run": firstRun})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, view)
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	cookie, _ := r.Cookie(auth.SessionCookieName)
	if err := s.Auth.RevokeSession(r.Context(), cookie.Value); err != nil {
		fail(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: auth.SessionCookieName, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: s.Auth.SecureRequest(r)})
	w.WriteHeader(204)
}

// GET /api/session: what /api/me says to someone signed in, and {signed_in:false} with a 200 to someone who is not. The page asks this on
// every load before it knows whether to show the sign-in, and a 401 there is a console error for each visit.
func (s *Server) sessionProbe(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(auth.SessionCookieName); err == nil {
		if _, err := s.Auth.CheckSession(r.Context(), cookie.Value); err == nil {
			s.Auth.RequireUser(http.HandlerFunc(s.me)).ServeHTTP(w, r)
			return
		}
	}
	writeJSON(w, 200, map[string]any{"signed_in": false})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u, err := s.Store.UserByID(r.Context(), userID(r))
	if err != nil {
		fail(w, err)
		return
	}
	cookie, _ := r.Cookie(auth.SessionCookieName)
	sess, err := s.Auth.Authenticate(r.Context(), cookie.Value)
	if err != nil {
		fail(w, err)
		return
	}
	firstRun, err := s.firstRun(r.Context(), u.ID)
	if err != nil {
		fail(w, err)
		return
	}
	view, err := s.sessionView(r.Context(), u.ID, map[string]any{"id": u.ID, "login": u.Login, "csrf_token": sess.CSRFToken, "first_run": firstRun})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, view)
}

// buildInfo answers GET /api/buildinfo: the version this server was built as and the Go toolchain that built it, for the header of the page
// (as v1 shows it). A signed-in account may read it; it names no path and no secret.
func (s *Server) buildInfo(w http.ResponseWriter, r *http.Request) {
	info := map[string]any{"version": s.Version, "go_version": runtime.Version()}
	if browser := telemetry.Current().BrowserConfig(); browser != nil {
		if id, err := s.telemetryReportID(r.Context(), userID(r)); err == nil {
			browser["user_id"] = id // the one id this person's reports carry: their fingerprint, or their login if they chose that
		}
		info["sentry"] = browser // the page starts its Sentry SDK from this; absent means it never loads the SDK
	}
	writeJSON(w, 200, info)
}

// sessionView adds what the account may do instance-wide (owner, admin) to a sign-in answer, so the page can turn off what would only
// answer 403: creating projects, and the owner's and admins' settings.
func (s *Server) sessionView(ctx context.Context, user string, view map[string]any) (map[string]any, error) {
	owner, err := s.Store.IsGlobalOwner(ctx, user)
	if err != nil {
		return nil, err
	}
	admin, err := s.Store.IsGlobalAdmin(ctx, user)
	if err != nil {
		return nil, err
	}
	view["owner"], view["admin"] = owner, admin
	// The consent popup for error reports is shown to the owner once, after the welcome on a new install and on the first sign-in after an update.
	if prompt, err := s.telemetryPrompt(ctx, owner); err != nil {
		return nil, err
	} else if prompt {
		view["telemetry_prompt"] = true
	}
	// Where the folder fields start: the home folder of the user the server runs as, which is the machine projects are created on. Only the
	// owner and admins create projects, so only they are told (a stranger gets no path of the host).
	if admin {
		if home, err := os.UserHomeDir(); err == nil && filepath.IsAbs(home) {
			view["host_home"] = home
		}
	}
	return view, nil
}

// firstRun says whether the first-run welcome is still to be shown to this user: only the owner, and only until they
// have been through it (an install that predates the welcome is marked seen by a migration).
func (s *Server) firstRun(ctx context.Context, user string) (bool, error) {
	owner, err := s.Store.IsGlobalOwner(ctx, user)
	if err != nil || !owner {
		return false, err
	}
	seen, err := s.Store.WelcomeSeen(ctx)
	return !seen, err
}

// POST /api/welcome/seen: the owner has been through the first-run welcome.
func (s *Server) welcomeSeen(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) {
		return
	}
	if err := s.Store.MarkWelcomeSeen(r.Context()); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) projects(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		p, err := s.Store.ProjectsForUser(r.Context(), userID(r))
		if err != nil {
			fail(w, err)
			return
		}
		views := make([]map[string]any, 0, len(p))
		for _, project := range p {
			views = append(views, projectView(project))
		}
		writeJSON(w, 200, views)
		return
	}
	if !s.adminMachine(w, r) {
		return
	}
	var req struct {
		Name    string `json:"name"`
		Folder  string `json:"folder"`
		RepoURL string `json:"repo_url"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Folder) == "" {
		apiError(w, 400, "invalid_request", "name and folder required")
		return
	}
	folder, err := checkProjectFolder(req.Folder)
	if err != nil {
		apiError(w, 400, "invalid_folder", err.Error())
		return
	}
	req.Folder = folder
	var p store.Project
	err = s.Store.Tx(r.Context(), func(tx *store.Tx) error {
		var err error
		p, err = tx.CreateProject(store.Project{Name: req.Name, Folder: req.Folder, RepoURL: req.RepoURL})
		if err != nil {
			return err
		}
		if err := tx.AddProjectOwner(p.ID, userID(r)); err != nil {
			return err
		}
		home, err := tx.CreateRoom(store.Room{ProjectID: p.ID, Kind: "home", Title: p.Name, CreatedBy: userID(r)})
		if err != nil {
			return err
		}
		// Nearly all work happens in a task room, and the home room is the lead's: every project starts with a "general" one beside it.
		_, err = tx.CreateRoom(store.Room{ProjectID: p.ID, ParentID: home.ID, Kind: "task", Title: store.GeneralRoomTitle, CreatedBy: userID(r)})
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, projectView(p))
}

// checkProjectFolder is the project folder as it will be stored: an absolute path to a folder that exists on the machine the server runs on
// (the host's project folder; other machines have their own folders set per machine). A relative path would mean whatever directory the
// server happened to start in.
func checkProjectFolder(folder string) (string, error) {
	folder = strings.TrimSpace(folder)
	if !filepath.IsAbs(folder) {
		return "", fmt.Errorf("the folder must be an absolute path, such as /home/you/repo (got %q)", folder)
	}
	clean := filepath.Clean(folder)
	info, err := os.Stat(clean)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s is not a folder on the machine running the server", clean)
	}
	return clean, nil
}

// projectUpdate renames a project or moves its folder. The project's owner or an admin may rename it; moving the folder points agents at
// a different place on the host, so it stays with the admins, as creating a project does. The folder can only change while the project has
// no agents or worktrees, because their checkouts live under it.
// projectOwnerOrAdmin lets a global admin or the project's owner through, and answers the refusal itself otherwise. admin says which.
func (s *Server) projectOwnerOrAdmin(w http.ResponseWriter, r *http.Request, id string) (allowed, admin bool) {
	admin, err := s.Store.IsGlobalAdmin(r.Context(), userID(r))
	if err != nil {
		fail(w, err)
		return false, false
	}
	if !admin {
		role, err := s.Store.UserProjectRole(r.Context(), userID(r), id)
		if err != nil {
			userError(w, err)
			return false, false
		}
		if role != "owner" {
			apiError(w, 403, "forbidden", "project owner or admin required")
			return false, false
		}
	}
	return true, admin
}

// roomRename sets a room's title (PATCH /api/rooms/{room} {title}). A task room's title is for the project's owners and operators, like
// creating a room; the home room's title is the project's name, so renaming it takes what renaming the project takes (its owner or an
// admin). A private chat has no title to change.
func (s *Server) roomRename(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomAccess(w, r, r.PathValue("room"))
	if !ok {
		return
	}
	var req struct {
		Title string `json:"title"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	switch room.Kind {
	case "dm":
		apiError(w, 409, "invalid_kind", "a private chat cannot be renamed")
		return
	case "home":
		if allowed, _ := s.projectOwnerOrAdmin(w, r, room.ProjectID); !allowed {
			return
		}
	default:
		if !s.projectOperator(w, r, room.ProjectID) {
			return
		}
	}
	title, err := store.NormalizeRoomTitle(req.Title)
	if err != nil {
		apiError(w, 400, "invalid_title", fmt.Sprintf("a room title is 1 to %d characters on one line", store.MaxRoomTitleRunes))
		return
	}
	renamed, err := s.Store.RenameRoom(r.Context(), room.ID, title, userID(r))
	switch {
	case errors.Is(err, store.ErrRoomNotRenamable):
		apiError(w, 409, "invalid_kind", "a private chat cannot be renamed")
	case err != nil:
		fail(w, err)
	default:
		writeJSON(w, 200, renamed)
	}
}

func (s *Server) projectUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project")
	allowed, admin := s.projectOwnerOrAdmin(w, r, id)
	if !allowed {
		return
	}
	var req struct {
		Name   *string `json:"name"`
		Folder *string `json:"folder"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Name == nil && req.Folder == nil {
		apiError(w, 400, "invalid_request", "name or folder required")
		return
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			apiError(w, 400, "invalid_request", "name cannot be empty")
			return
		}
		req.Name = &name
	}
	if req.Folder != nil {
		current, err := s.Store.ProjectFolder(r.Context(), id)
		if errors.Is(err, sql.ErrNoRows) {
			apiError(w, 404, "not_found", "no such project")
			return
		}
		if err != nil {
			fail(w, err)
			return
		}
		// Sending the folder it already has is not a change (a form can send both fields), and is not checked: a project from before
		// folders were checked may hold one that no longer exists.
		if strings.TrimSpace(*req.Folder) == current {
			req.Folder = nil
		} else {
			if !admin {
				apiError(w, 403, "forbidden", "admin required to change a project's folder")
				return
			}
			folder, err := checkProjectFolder(*req.Folder)
			if err != nil {
				apiError(w, 400, "invalid_folder", err.Error())
				return
			}
			req.Folder = &folder
		}
	}
	if req.Name == nil && req.Folder == nil {
		project, err := s.Store.ProjectByID(r.Context(), id)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, projectView(project))
		return
	}
	project, err := s.Store.UpdateProject(r.Context(), id, req.Name, req.Folder, userID(r))
	switch {
	case errors.Is(err, store.ErrProjectFolderInUse):
		apiError(w, 409, "folder_in_use", "this project already has agents or worktrees under its folder, so the folder cannot change")
	case errors.Is(err, sql.ErrNoRows):
		apiError(w, 404, "not_found", "no such project")
	case err != nil:
		fail(w, err)
	default:
		writeJSON(w, 200, projectView(project))
	}
}
func (s *Server) rooms(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if !s.projectAccess(w, r, project) {
		return
	}
	if r.Method == http.MethodGet {
		rooms, err := s.Store.RoomsForUserProject(r.Context(), project, userID(r))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, rooms)
		return
	}
	if !s.projectOperator(w, r, project) {
		return
	}
	var req struct {
		ParentID string          `json:"parent_id"`
		Kind     string          `json:"kind"`
		Title    string          `json:"title"`
		Worktree worktreeRequest `json:"worktree"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		apiError(w, 400, "invalid_request", "title required")
		return
	}
	if req.Kind == "" {
		req.Kind = "task"
	}
	if req.Kind != "task" {
		apiError(w, 400, "invalid_kind", "only task rooms can be created")
		return
	}
	if req.Worktree.Enabled {
		role, err := s.Store.UserProjectRole(r.Context(), userID(r), project)
		if err != nil {
			fail(w, err)
			return
		}
		if role != "owner" && role != "operator" {
			apiError(w, 403, "forbidden", "project owner or operator required for worktree creation")
			return
		}
	}
	if req.ParentID != "" {
		parent, ok := s.roomAccess(w, r, req.ParentID)
		if !ok {
			return
		}
		if parent.ProjectID != project {
			apiError(w, 400, "invalid_request", "parent belongs to another project")
			return
		}
		if parent.Kind == "dm" {
			apiError(w, 400, "invalid_kind", "private chats cannot have child rooms")
			return
		}
	}
	room, err := s.Store.CreateRoom(r.Context(), store.Room{ProjectID: project, ParentID: req.ParentID, Kind: req.Kind, Title: req.Title, CreatedBy: userID(r)})
	if err != nil {
		if errors.Is(err, store.ErrDepthExceeded) {
			apiError(w, 400, "depth_exceeded", "room depth exceeded")
		} else if errors.Is(err, store.ErrInvalidTransition) {
			apiError(w, 409, "room_closed", "parent room is closing or closed")
		} else {
			fail(w, err)
		}
		return
	}
	if req.Worktree.Enabled {
		host, hostErr := s.Store.HostMachine(r.Context())
		if hostErr != nil {
			fail(w, hostErr)
			return
		}
		item, workErr := s.ensureRoomWorktree(r.Context(), room, host, req.Worktree.BaseRef)
		if workErr != nil {
			item.State, item.Reason = "failed", workErr.Error()
		}
		room.Worktree = &item
	}
	writeJSON(w, 201, room)
}
func (s *Server) room(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomAccess(w, r, r.PathValue("room"))
	if !ok {
		return
	}
	room, err := s.Store.RoomForUserProject(r.Context(), room.ProjectID, userID(r), room.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, room)
}
func (s *Server) roomMentionUsers(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomAccess(w, r, r.PathValue("room"))
	if !ok {
		return
	}
	logins, err := s.Store.RoomUserLogins(r.Context(), room.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"logins": logins})
}

// roomMembers is the read-only member list of a room (people, agents with their role label and
// emoji, singleton roles and their holders). Same access as the room itself; nothing is changed.
func (s *Server) roomMembers(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomAccess(w, r, r.PathValue("room"))
	if !ok {
		return
	}
	members, err := s.Store.RoomMembers(r.Context(), room.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, members)
}
func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomAccess(w, r, r.PathValue("room"))
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		query := r.URL.Query()
		limit := 200
		if raw := query.Get("limit"); query.Has("limit") {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 1 || value > 500 {
				apiError(w, 400, "invalid_request", "limit must be between 1 and 500")
				return
			}
			limit = value
		}
		if query.Has("after") && (query.Has("before") || query.Has("around")) || query.Has("before") && query.Has("around") {
			apiError(w, 400, "invalid_request", "message cursors cannot be combined")
			return
		}
		var messages []store.Message
		var err error
		switch {
		case query.Has("thread_root"):
			rootID := query.Get("thread_root")
			if rootID == "" {
				apiError(w, 400, "invalid_request", "thread root is empty")
				return
			}
			root, rootErr := s.Store.MessageByID(r.Context(), rootID)
			if rootErr != nil || root.RoomID != room.ID {
				if rootErr != nil && !errors.Is(rootErr, sql.ErrNoRows) {
					fail(w, rootErr)
				} else {
					apiError(w, 404, "not_found", "thread root not found in room")
				}
				return
			}
			// Any message of a conversation opens the whole conversation: a reply names its first message as thread_root.
			if root.ThreadRoot != "" {
				rootID = root.ThreadRoot
			}
			switch {
			case query.Has("after"):
				var after int64
				after, err = parseAfter(r)
				if err != nil {
					apiError(w, 400, "invalid_request", "invalid after cursor")
					return
				}
				messages, err = s.Store.ListThreadRepliesAfter(r.Context(), room.ID, rootID, after, limit)
			case query.Has("before"), query.Has("around"):
				key := query.Get("before")
				if query.Has("around") {
					key = query.Get("around")
				}
				if key == "" {
					apiError(w, 400, "invalid_request", "message cursor is empty")
					return
				}
				var seq int64
				seq, err = strconv.ParseInt(key, 10, 64)
				if err == nil && seq < 1 {
					apiError(w, 400, "invalid_request", "message cursor must be positive")
					return
				}
				if err != nil {
					seq, err = s.Store.MessageSeqInRoom(r.Context(), room.ID, key)
				}
				if err == nil {
					if query.Has("around") {
						messages, err = s.Store.ListThreadRepliesAround(r.Context(), room.ID, rootID, seq, limit)
					} else {
						messages, err = s.Store.ListThreadRepliesBefore(r.Context(), room.ID, rootID, seq, limit)
					}
				}
			default:
				messages, err = s.Store.ListThreadRepliesLatest(r.Context(), room.ID, rootID, limit)
			}
		case query.Has("after"):
			var after int64
			after, err = parseAfter(r)
			if err != nil {
				apiError(w, 400, "invalid_request", "invalid after cursor")
				return
			}
			messages, err = s.Store.ListMessagesAfterLimit(r.Context(), room.ID, after, limit)
		case query.Has("before"), query.Has("around"):
			key := query.Get("before")
			if query.Has("around") {
				key = query.Get("around")
			}
			if key == "" {
				apiError(w, 400, "invalid_request", "message cursor is empty")
				return
			}
			var seq int64
			seq, err = strconv.ParseInt(key, 10, 64)
			if err == nil && seq < 1 {
				apiError(w, 400, "invalid_request", "message cursor must be positive")
				return
			}
			if err != nil {
				seq, err = s.Store.MessageSeqInRoom(r.Context(), room.ID, key)
			}
			if err == nil {
				if query.Has("around") {
					messages, err = s.Store.ListMessagesAround(r.Context(), room.ID, seq, limit)
				} else {
					messages, err = s.Store.ListMessagesBefore(r.Context(), room.ID, seq, limit)
				}
			}
		default:
			messages, err = s.Store.ListMessagesLatest(r.Context(), room.ID, limit)
		}
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				apiError(w, 404, "not_found", "message cursor not found in room")
				return
			}
			fail(w, err)
			return
		}
		if messages == nil {
			messages = []store.Message{}
		}
		reactions, err := s.Store.ReactionsForMessages(r.Context(), messages)
		if err != nil {
			fail(w, err)
			return
		}
		threadCounts, err := s.Store.ThreadCountsForMessages(r.Context(), room.ID, messages)
		if err != nil {
			fail(w, err)
			return
		}
		ids := make([]string, 0, len(messages))
		for _, message := range messages {
			ids = append(ids, message.ID)
		}
		groupOnly, err := s.Store.GroupOnlyMessages(r.Context(), ids)
		if err != nil {
			fail(w, err)
			return
		}
		previews, err := s.Store.ReplyPreviewsForMessages(r.Context(), room.ID, messages)
		if err != nil {
			fail(w, err)
			return
		}
		usages, err := s.Store.UsageForMessages(r.Context(), messages)
		if err != nil {
			fail(w, err)
			return
		}
		views := make([]map[string]any, 0, len(messages))
		for _, message := range messages {
			view := messageView(message)
			if preview, ok := previews[message.ReplyTo]; ok && message.ReplyTo != "" {
				view["reply_preview"] = preview
			}
			if usage, ok := usages[message.ID]; ok {
				view["usage"] = usage
			}
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
			views = append(views, view)
		}
		writeJSON(w, 200, views)
		return
	}
	if !s.roomWriteAccess(w, r, room) {
		return
	}
	state, err := s.Store.RoomLifecycle(r.Context(), room.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if state != "open" {
		apiError(w, 409, "room_"+state, "room is "+state)
		return
	}
	var req struct {
		Body        string   `json:"body"`
		ReplyTo     string   `json:"reply_to"`
		ThreadRoot  string   `json:"thread_root"` // read and ignored: the conversation follows reply_to
		Attachments []string `json:"attachments"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !utf8.ValidString(req.Body) || len(req.Body) > 32<<10 || strings.TrimSpace(req.Body) == "" && len(req.Attachments) == 0 {
		apiError(w, 400, "invalid_answer", "invalid body")
		return
	}
	// One concept, the reply: thread_root is derived from reply_to, so one in the body (an older client) is read and ignored.
	if req.ReplyTo != "" {
		parent, err := s.Store.MessageByID(r.Context(), req.ReplyTo)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				apiError(w, 400, "invalid_answer", "reply target not found")
			} else {
				fail(w, err)
			}
			return
		}
		if parent.RoomID != room.ID {
			apiError(w, 400, "invalid_answer", "reply belongs to another room")
			return
		}
	}
	m, err := s.Store.InsertMessage(r.Context(), store.Message{RoomID: room.ID, AuthorKind: "user", AuthorID: userID(r), Body: req.Body, ReplyTo: req.ReplyTo, AttachmentIDs: req.Attachments})
	if err != nil {
		if errors.Is(err, store.ErrInvalidAttachment) {
			apiError(w, 400, "invalid_attachment", "invalid attachment")
			return
		}
		fail(w, err)
		return
	}
	if m.AuthorKind == "user" {
		if author, err := s.Store.UserByID(r.Context(), m.AuthorID); err == nil {
			m.AuthorName = author.Login
		}
	}
	view := messageView(m)
	if m.ReplyTo != "" {
		// The message is saved: failing now would make a retry post it twice. A reply without its preview is quoted from the page instead.
		if previews, err := s.Store.ReplyPreviewsForMessages(r.Context(), room.ID, []store.Message{m}); err != nil {
			log.Printf("reply preview for saved message %s: %v", m.ID, err)
		} else if preview, ok := previews[m.ReplyTo]; ok {
			view["reply_preview"] = preview
		}
	}
	groupOnly, err := s.Store.GroupOnlyMessages(r.Context(), []string{m.ID})
	if err != nil {
		fail(w, err)
		return
	}
	view["expirable"] = store.ExpirableMessage(m.AuthorKind, m.ReplyTo, room.Kind, groupOnly[m.ID])
	if m.ThreadRoot != "" {
		counts, err := s.threadCountsForMessages(r.Context(), room.ID, []store.Message{m})
		if err != nil {
			log.Printf("thread count for saved message %s: %v", m.ID, err)
		} else {
			view["thread_count"] = counts[m.ThreadRoot]
		}
	}
	writeJSON(w, 201, view)
}

func (s *Server) threadCountsForMessages(ctx context.Context, roomID string, messages []store.Message) (map[string]int, error) {
	if s.threadCountsQuery != nil {
		return s.threadCountsQuery(ctx, roomID, messages)
	}
	return s.Store.ThreadCountsForMessages(ctx, roomID, messages)
}

func (s *Server) answerInbox(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Body string `json:"body"`
		Rev  int    `json:"rev"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !utf8.ValidString(req.Body) || strings.TrimSpace(req.Body) == "" || len(req.Body) > 32<<10 || req.Rev < 0 {
		apiError(w, 400, "invalid_answer", "invalid answer")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 128 {
		apiError(w, 400, "invalid_request", "Idempotency-Key required")
		return
	}
	message, reused, err := s.Store.AnswerInbox(r.Context(), r.PathValue("item"), userID(r), key, req.Body, req.Rev)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrConflict):
			apiError(w, 409, "conflict", "request key reused with different answer")
		case errors.Is(err, store.ErrAlreadyResolved):
			apiError(w, 409, "already_resolved", "inbox item is resolved")
		case errors.Is(err, store.ErrAlreadyWithdrawn):
			apiError(w, 409, "already_withdrawn", "inbox item is withdrawn")
		case errors.Is(err, store.ErrStaleInboxRev):
			apiError(w, 409, "stale_rev", "inbox item revision changed")
		case errors.Is(err, store.ErrInvalidTransition):
			apiError(w, 409, "room_closed", "room is closed")
		default:
			fail(w, err)
		}
		return
	}
	status := 201
	if reused {
		status = 200
	}
	if message.AuthorKind == "user" {
		if author, err := s.Store.UserByID(r.Context(), message.AuthorID); err == nil {
			message.AuthorName = author.Login
		}
	}
	writeJSON(w, status, messageView(message))
}
func (s *Server) reactions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("message")
	message, err := s.Store.MessageByID(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	room, ok := s.roomAccess(w, r, message.RoomID)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		items, err := s.Store.ReactionsForMessage(r.Context(), id)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, items)
		return
	}
	if !s.roomWriteAccess(w, r, room) {
		return
	}
	emoji := r.PathValue("emoji")
	if r.Method == http.MethodPut && !validReactionEmoji(emoji) || r.Method == http.MethodDelete && !validReactionRemoval(emoji) {
		apiError(w, 400, "invalid_request", "invalid emoji")
		return
	}
	err = s.Store.Tx(r.Context(), func(tx *store.Tx) error {
		return tx.SetReaction(id, "user", userID(r), emoji, r.Method == http.MethodPut)
	})
	if err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(204)
}
func parseAfter(r *http.Request) (int64, error) {
	value := r.URL.Query().Get("after")
	if value == "" {
		return 0, nil
	}
	seq, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seq < 0 {
		return 0, fmt.Errorf("invalid cursor")
	}
	return seq, nil
}

// eventAcceptOptions lets the WebSocket library accept the configured public
// origin too. CheckWSOrigin has already enforced the origin policy; without
// this, a proxy that rewrites Host to the upstream address would still fail
// the library's own Origin-equals-Host check.
func (s *Server) eventAcceptOptions() *websocket.AcceptOptions {
	public := s.Auth.PublicOrigin()
	if public == "" {
		return nil
	}
	escaped := strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`).Replace(public)
	return &websocket.AcceptOptions{OriginPatterns: []string{escaped}}
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if !s.Auth.CheckWSOrigin(r) {
		apiError(w, 403, "forbidden", "forbidden origin")
		return
	}
	after, err := parseAfter(r)
	if err != nil {
		apiError(w, 400, "invalid_request", "invalid after cursor")
		return
	}
	conn, err := websocket.Accept(w, r, s.eventAcceptOptions())
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	ctx, cancel := context.WithCancel(conn.CloseRead(r.Context()))
	defer cancel()
	cookie, _ := r.Cookie(auth.SessionCookieName)
	s.streamEvents(ctx, conn, userID(r), cookie.Value, after)
}

func (s *Server) eventCursor(w http.ResponseWriter, r *http.Request) {
	_, seq, err := s.Store.EventBounds(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"seq": seq})
}
func eventFrame(event store.Event) map[string]any {
	return map[string]any{"type": "event", "event": event}
}
func (s *Server) writeFrame(ctx context.Context, conn *websocket.Conn, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	timeout := s.WriteTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, raw)
}
