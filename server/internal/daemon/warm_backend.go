package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/pelletier/go-toml/v2"
)

type warmHostFactory func(context.Context, agent.Config, agent.ExecOptions) (agent.WarmHost, error)

type pooledCodexBackend struct {
	pool        *warmSessionPool
	key         string
	fingerprint string
	factory     func(context.Context) (agent.WarmHost, error)
	fallback    agent.Backend
	logger      interface {
		Debug(string, ...any)
		Warn(string, ...any)
	}
}

func (b *pooledCodexBackend) Execute(ctx context.Context, prompt string, opts agent.ExecOptions) (*agent.Session, error) {
	lease, hit, err := b.pool.acquire(ctx, b.key, b.fingerprint, b.factory)
	if err != nil {
		if b.fallback != nil &&
			!errors.Is(err, context.Canceled) &&
			!errors.Is(err, context.DeadlineExceeded) &&
			!errors.Is(err, errWarmSessionCleanupUnconfirmed) {
			b.logger.Warn("codex warm host unavailable; using cold backend", "error", err)
			return b.fallback.Execute(ctx, prompt, opts)
		}
		return nil, err
	}
	host := lease.Host()
	if !host.Healthy() {
		if releaseErr := lease.Release(ctx, false); releaseErr != nil {
			return nil, releaseErr
		}
		// An app-server can exit while idle. Rebuild once before the turn has
		// been submitted; after Execute starts, replay is never implicit.
		lease, hit, err = b.pool.acquire(ctx, b.key, b.fingerprint, b.factory)
		if err != nil {
			return nil, err
		}
		host = lease.Host()
		if !host.Healthy() {
			_ = lease.Release(context.Background(), false)
			return nil, errors.New("warm pool replacement host is not healthy")
		}
	}
	b.logger.Debug("codex warm pool lease acquired", "hit", hit, "key_hash", shortWarmHash(b.key))
	session, err := host.Execute(ctx, prompt, opts)
	if err != nil {
		_ = lease.Release(context.Background(), false)
		return nil, err
	}
	messageCh := make(chan agent.Message, 256)
	resultCh := make(chan agent.Result, 1)
	go func() {
		defer close(messageCh)
		defer close(resultCh)
		attemptOpts := opts
		for attempt := 1; attempt <= 2; attempt++ {
			var heldPins []agent.Message
			holdingPins := true
			for msg := range session.Messages {
				if holdingPins && msg.Type == agent.MessageStatus && msg.Status == "running" {
					heldPins = append(heldPins, msg)
					continue
				}
				if holdingPins {
					forwardWarmMessages(messageCh, heldPins)
					heldPins = nil
					holdingPins = false
				}
				forwardWarmMessages(messageCh, []agent.Message{msg})
			}
			result, ok := <-session.Result
			if !ok {
				result = agent.Result{Status: "failed", Error: "warm host closed without result"}
			}
			retryCandidate := attempt == 1 && ok && agent.CodexWarmStartupRetryCandidate(result)
			reusable := ok && host.Healthy() && result.Status != "timeout" && result.Status != "aborted"
			if reusable && !retryCandidate {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Second)
				cleanupErr := host.PrepareIdle(cleanupCtx)
				cleanupCancel()
				if cleanupErr != nil {
					reusable = false
					b.logger.Warn("codex warm host idle preparation failed", "error", cleanupErr)
				}
			}
			releaseErr := lease.Release(context.Background(), reusable && !retryCandidate)
			if releaseErr != nil {
				b.logger.Warn("codex warm pool release failed", "error", releaseErr, "reusable", reusable)
				result.Status = "failed"
				if result.Error == "" {
					result.Error = releaseErr.Error()
				} else {
					result.Error = errors.Join(errors.New(result.Error), releaseErr).Error()
				}
			}
			if !retryCandidate || releaseErr != nil || ctx.Err() != nil {
				if holdingPins {
					forwardWarmMessages(messageCh, heldPins)
				}
				resultCh <- result
				return
			}

			// Match the cold backend's single safe retry. No non-pin event was
			// forwarded, and Release(false) synchronously confirmed the old tree
			// is gone, so replay cannot race a surviving turn.
			backoff := 500*time.Millisecond + time.Duration(time.Now().UnixNano()%1000)*time.Millisecond
			b.logger.Warn("codex warm host retry scheduled", "reason", "model_catalog_refresh", "backoff", backoff.String())
			select {
			case <-ctx.Done():
				forwardWarmMessages(messageCh, heldPins)
				resultCh <- result
				return
			case <-time.After(backoff):
			}
			if attemptOpts.ResumeSessionID != "" {
				attemptOpts.ResumeSessionID = ""
				attemptOpts.ResumeExpected = true
			}
			lease, _, err = b.pool.acquire(ctx, b.key, b.fingerprint, b.factory)
			if err != nil {
				b.logger.Warn("codex warm host retry acquisition failed", "error", err)
				forwardWarmMessages(messageCh, heldPins)
				resultCh <- result
				return
			}
			host, ok = lease.Host().(agent.WarmHost)
			if !ok {
				_ = lease.Release(context.Background(), false)
				forwardWarmMessages(messageCh, heldPins)
				resultCh <- result
				return
			}
			session, err = host.Execute(ctx, prompt, attemptOpts)
			if err != nil {
				_ = lease.Release(context.Background(), false)
				b.logger.Warn("codex warm host retry execute failed", "error", err)
				forwardWarmMessages(messageCh, heldPins)
				resultCh <- result
				return
			}
		}
	}()
	return &agent.Session{Messages: messageCh, Result: resultCh}, nil
}

func forwardWarmMessages(dst chan<- agent.Message, messages []agent.Message) {
	for _, msg := range messages {
		select {
		case dst <- msg:
		default:
		}
	}
}

type pinnedWarmHost struct {
	agent.WarmHost
	onClose func()
	once    sync.Once
}

func (h *pinnedWarmHost) Close(ctx context.Context) error {
	err := h.WarmHost.Close(ctx)
	if err == nil {
		h.once.Do(h.onClose)
	}
	return err
}

func (d *Daemon) pooledCodexBackend(task Task, cfg agent.Config, opts agent.ExecOptions, env *execenv.Environment, usesCustomProfile bool, fallback agent.Backend) agent.Backend {
	if !codexWarmEligible(task, opts, env, usesCustomProfile) {
		return nil
	}
	pool := d.codexWarmSessionPool()
	key := codexWarmKey(task)
	fingerprint := codexWarmFingerprint(task, cfg, opts, env)
	return &pooledCodexBackend{
		pool: pool, key: key, fingerprint: fingerprint, logger: d.logger, fallback: fallback,
		factory: func(ctx context.Context) (agent.WarmHost, error) {
			// The task's ordinary active-root reference ends when runTask returns;
			// retain a second reference for the app-server's whole idle lifetime so
			// GC cannot remove CODEX_HOME or the cwd beneath a warm process.
			d.markActiveEnvRoot(env.RootDir)
			host, err := d.newWarmHost(ctx, cfg, opts)
			if err != nil {
				d.unmarkActiveEnvRoot(env.RootDir)
				return nil, err
			}
			return &pinnedWarmHost{WarmHost: host, onClose: func() { d.unmarkActiveEnvRoot(env.RootDir) }}, nil
		},
	}
}

func codexWarmEligible(task Task, opts agent.ExecOptions, env *execenv.Environment, usesCustomProfile bool) bool {
	if runtime.GOOS == "windows" || env == nil || env.RootDir == "" || env.CodexHome == "" || usesCustomProfile {
		return false
	}
	if task.IssueID == "" && task.ChatSessionID == "" {
		return false
	}
	// Task-scoped brokers and MCP subprocesses may capture credentials outside
	// shell_environment_policy. Keep them on the proven cold lifecycle until
	// those transports gain their own per-turn credential refresh contract.
	if len(task.RemoteMCPConnections) != 0 || len(task.ConnectedApps) != 0 || len(opts.McpConfig) != 0 {
		return false
	}
	if agent.CodexArgsConfigureMCP(opts.ExtraArgs) || agent.CodexArgsConfigureMCP(opts.CustomArgs) || codexConfigHasMCP(env.CodexHome) {
		return false
	}
	return true
}

// codexConfigHasMCP detects inherited config.toml MCP processes. Their
// environment is owned by app-server startup rather than the per-thread shell
// policy, so retaining one across task-token rotations is not safe yet. Read or
// parse errors fail closed onto the ordinary one-shot backend.
func codexConfigHasMCP(codexHome string) bool {
	data, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		return true
	}
	var cfg map[string]any
	if toml.Unmarshal(data, &cfg) != nil {
		return true
	}
	mcp, exists := cfg["mcp_servers"]
	if !exists {
		return false
	}
	servers, ok := mcp.(map[string]any)
	return !ok || len(servers) > 0
}

func codexWarmKey(task Task) string {
	conversation := task.IssueID
	if task.ChatSessionID != "" {
		conversation = "chat:" + task.ChatSessionID
	}
	return strings.Join([]string{task.WorkspaceID, task.RuntimeID, task.AgentID, conversation}, "\x00")
}

func codexWarmFingerprint(task Task, cfg agent.Config, opts agent.ExecOptions, env *execenv.Environment) string {
	// Dynamic task identity/credentials deliberately stay out. Their current
	// values are carried by ExecOptions.CodexShellEnv on every resume.
	payload := struct {
		Executable, CLIVersion, CodexVersion            string
		LaunchPrefix, ExtraArgs, CustomArgs             []string
		Model, Thinking, Tier                           string
		Root, WorkDir, CodexHome                        string
		Agent                                           *AgentData
		PluginDigest                                    string
		WorkspaceContext, ProjectID, ProjectDescription string
		RequestingUserProfile                           string
		CodexConfigDigest                               string
	}{
		Executable: cfg.ExecutablePath, CLIVersion: cfg.CLIVersion, CodexVersion: cfg.CodexVersion,
		LaunchPrefix: cfg.LaunchPrefix, ExtraArgs: opts.ExtraArgs, CustomArgs: opts.CustomArgs,
		Model: opts.Model, Thinking: opts.ThinkingLevel, Tier: opts.ServiceTier,
		Root: env.RootDir, WorkDir: env.WorkDir, CodexHome: env.CodexHome, Agent: task.Agent,
		WorkspaceContext: task.WorkspaceContext, ProjectID: task.ProjectID,
		ProjectDescription:    task.ProjectDescription,
		RequestingUserProfile: task.RequestingUserProfileDescription,
		CodexConfigDigest:     fileContentDigest(filepath.Join(env.CodexHome, "config.toml")),
	}
	if task.PluginExecutionManifest != nil {
		payload.PluginDigest = task.PluginExecutionManifest.SnapshotDigest
	}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func fileContentDigest(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "unreadable:" + err.Error()
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func shortWarmHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:6])
}

func codexTurnShellEnvironment(inherited []string, explicit map[string]string, authorizedExplicit []string) map[string]string {
	allowed := execenv.CodexShellEnvAllowlist(inherited, explicit, authorizedExplicit)
	merged := make(map[string]string, len(inherited)+len(explicit))
	canonical := make(map[string]string, len(inherited)+len(explicit))
	for _, entry := range inherited {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			canonical[strings.ToUpper(key)] = key
			merged[key] = value
		}
	}
	for key, value := range explicit {
		if prior := canonical[strings.ToUpper(key)]; prior != "" && prior != key {
			delete(merged, prior)
		}
		canonical[strings.ToUpper(key)] = key
		merged[key] = value
	}
	out := make(map[string]string, len(allowed))
	for _, key := range allowed {
		actual := canonical[strings.ToUpper(key)]
		if actual == "" {
			actual = key
		}
		if value, ok := merged[actual]; ok {
			out[actual] = value
		}
	}
	return out
}

func (d *Daemon) codexWarmSessionPool() *warmSessionPool {
	d.warmPoolMu.Lock()
	defer d.warmPoolMu.Unlock()
	if d.codexWarmPool == nil {
		d.codexWarmPool = newWarmSessionPool(defaultWarmSessionMaxPerProvider, defaultWarmSessionIdleTTL)
	}
	return d.codexWarmPool
}

func (d *Daemon) codexConversationLocker() *LocalPathLocker {
	d.warmPoolMu.Lock()
	defer d.warmPoolMu.Unlock()
	if d.warmConversationLocks == nil {
		d.warmConversationLocks = NewLocalPathLocker()
	}
	return d.warmConversationLocks
}

func (d *Daemon) newWarmHost(ctx context.Context, cfg agent.Config, opts agent.ExecOptions) (agent.WarmHost, error) {
	if d.warmHostFactory != nil {
		return d.warmHostFactory(ctx, cfg, opts)
	}
	return agent.NewWarmHost(ctx, "codex", cfg, opts)
}

func (d *Daemon) warmPoolSweepLoop(ctx context.Context) {
	ticker := time.NewTicker(defaultWarmSessionSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.codexWarmSessionPool().sweep(context.Background()); err != nil {
				d.logger.Warn("codex warm pool sweep failed", "error", err)
			}
		}
	}
}

func (d *Daemon) closeWarmPools() {
	d.warmPoolMu.Lock()
	pool := d.codexWarmPool
	d.warmPoolMu.Unlock()
	if pool == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := pool.close(ctx); err != nil {
		d.logger.Warn("codex warm pool shutdown incomplete", "error", err)
	}
}
