package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// codexWarmHost owns one initialized app-server process. The daemon leases a
// host to exactly one conversation at a time; the additional active CAS keeps
// misuse at the package boundary from multiplexing two turns accidentally.
type codexWarmHost struct {
	cfg        Config
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	client     *codexClient
	cancel     context.CancelFunc
	readerDone chan struct{}
	waitDone   chan struct{}
	stderr     *stderrTail

	active            atomic.Pointer[codexWarmTurn]
	poison            atomic.Bool
	closed            atomic.Bool
	closeOnce         sync.Once
	waitErr           error
	cleanupErr        error
	baselineProcesses map[int]struct{}
}

type codexWarmTurn struct {
	mu               sync.Mutex
	messages         chan Message
	done             chan bool
	activity         chan string
	closed           bool
	finalAnswer      string
	lastAgentMessage string
	gate             *codexTurnNotificationGate
}

func (t *codexWarmTurn) emit(msg Message) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	if msg.Type == MessageText {
		t.lastAgentMessage = msg.Content
	}
	select {
	case t.messages <- msg:
	default:
	}
	select {
	case t.activity <- describeCodexSemanticActivity(msg):
	default:
	}
}

func (t *codexWarmTurn) semantic(description string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		select {
		case t.activity <- description:
		default:
		}
	}
}

func (t *codexWarmTurn) complete(aborted bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		select {
		case t.done <- aborted:
		default:
		}
	}
}

func (t *codexWarmTurn) final(text string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.finalAnswer = text
	}
}

func (t *codexWarmTurn) finish() (string, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.closed = true
		close(t.messages)
	}
	return t.finalAnswer, t.lastAgentMessage
}

func newCodexWarmHost(startupCtx context.Context, cfg Config, opts ExecOptions) (*codexWarmHost, error) {
	for attempt := 1; attempt <= 2; attempt++ {
		host, err := newCodexWarmHostOnce(startupCtx, cfg, opts)
		if err == nil {
			return host, nil
		}
		var handshakeErr *codexHandshakeTimeoutError
		if attempt == 2 || !errors.As(err, &handshakeErr) || handshakeErr.Method != "initialize" || !codexInitializeRetrySupported() {
			return nil, err
		}
		backoff := 75*time.Millisecond + time.Duration(time.Now().UnixNano()%50)*time.Millisecond
		cfg.Logger.Warn("codex warm host initialize retry scheduled", "attempt", attempt, "backoff", backoff.String())
		select {
		case <-startupCtx.Done():
			return nil, context.Cause(startupCtx)
		case <-time.After(backoff):
		}
	}
	return nil, errors.New("codex warm host failed to initialize")
}

func newCodexWarmHostOnce(startupCtx context.Context, cfg Config, opts ExecOptions) (*codexWarmHost, error) {
	if !codexWarmHostingSupported() {
		return nil, errors.New("codex warm hosting is unavailable: selective child cleanup is unsupported")
	}
	execPath := cfg.ExecutablePath
	if execPath == "" {
		execPath = "codex"
	}
	if _, err := exec.LookPath(execPath); err != nil {
		return nil, fmt.Errorf("codex executable not found at %q: %w", execPath, err)
	}
	codexHome := strings.TrimSpace(cfg.Env["CODEX_HOME"])
	if codexHome != "" {
		if err := ensureCodexMcpConfig(filepath.Join(codexHome, "config.toml"), opts.McpConfig, cfg.Logger); err != nil {
			return nil, fmt.Errorf("apply codex mcp_config: %w", err)
		}
	} else if hasManagedCodexMcpConfig(opts.McpConfig) {
		return nil, errors.New("codex: mcp_config is set but CODEX_HOME env var is not configured; cannot apply managed MCP")
	}

	runtimeCmd := cfg.commandAt(execPath)
	if codexHome != "" {
		opts.ExtraArgs = filterCodexShellEnvConfigOverrides(opts.ExtraArgs, cfg.Logger)
		opts.CustomArgs = filterCodexShellEnvConfigOverrides(opts.CustomArgs, cfg.Logger)
		runtimeCmd = runtimeCmd.withFilteredPrefix(func(prefix []string) []string {
			return filterCodexShellEnvConfigOverrides(prefix, cfg.Logger)
		})
	}
	if hasManagedCodexMcpConfig(opts.McpConfig) {
		runtimeCmd = runtimeCmd.withFilteredPrefix(func(prefix []string) []string {
			return filterCodexCustomConfigOverrides(prefix, cfg.Logger)
		})
	}
	if opts.ServiceTier == codexFastServiceTier {
		runtimeCmd = runtimeCmd.withFilteredPrefix(func(prefix []string) []string {
			return stripCodexFastModeConflicts(prefix, cfg.Logger)
		})
	}

	hostCtx, cancel := context.WithCancel(context.Background())
	cmd := runtimeCmd.exec(hostCtx, buildCodexArgs(opts, cfg.Logger)...)
	hideAgentWindow(cmd)
	configureProcessGroup(cmd)
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			signalProcessGroup(cmd, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = codexProcessWaitDelay()
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	// The app-server outlives an individual task. Keep task identity,
	// credentials and task-local temp paths out of its own environment; every
	// shell tool receives the current values through thread/start or
	// thread/resume instead. Sanitise cfg itself as well so the host cannot
	// accidentally reuse stale values through a later code path.
	cfg.Env = codexWarmSanitizedEnvironment(cfg.Env)
	cmd.Env = codexWarmHostEnvironment(cfg.Env)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("codex stdout pipe: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("codex stdin pipe: %w", err)
	}
	stderrBuf := newStderrTail(io.Discard, codexStderrTailBytes)
	cmd.Stderr = stderrBuf
	if err := startOwnedProcessTree(cmd, cfg.Logger); err != nil {
		cancel()
		return nil, fmt.Errorf("start codex: %w", err)
	}
	active := activeCodexLaunches.Add(1)
	for {
		maxSeen := maxActiveCodexLaunchesObserved.Load()
		if active <= maxSeen || maxActiveCodexLaunchesObserved.CompareAndSwap(maxSeen, active) {
			break
		}
	}

	h := &codexWarmHost{
		cfg: cfg, cmd: cmd, stdin: stdin, cancel: cancel,
		readerDone: make(chan struct{}), waitDone: make(chan struct{}), stderr: stderrBuf,
	}
	handshakeTimeout, threadHandshakeTimeout := resolveCodexHandshakeTimeouts(opts)
	h.client = &codexClient{
		cfg: cfg, stdin: stdin, pending: make(map[int]*pendingRPC),
		processDone: make(chan struct{}), handshakeTimeout: handshakeTimeout,
		threadHandshakeTimeout: threadHandshakeTimeout,
		pid:                    cmd.Process.Pid, activeLaunches: active, notificationProtocol: "unknown",
	}
	h.client.acceptNotification = func(method string, params map[string]any) bool {
		t := h.active.Load()
		return t != nil && t.gate.accept(method, params)
	}
	h.client.onMessage = func(msg Message) {
		logCodexAgentMessage(cfg.Logger, msg)
		if t := h.active.Load(); t != nil {
			t.emit(msg)
		}
	}
	h.client.onSemanticActivity = func(description string) {
		if t := h.active.Load(); t != nil {
			t.semantic(description)
		}
	}
	h.client.onTurnDone = func(aborted bool) {
		if t := h.active.Load(); t != nil {
			t.complete(aborted)
		}
	}
	h.client.onFinalAnswer = func(text string) {
		if t := h.active.Load(); t != nil {
			t.final(text)
		}
	}

	go func() {
		defer close(h.readerDone)
		scanner := newAgentStreamScanner(stdout)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" {
				h.client.handleLine(line)
			}
		}
		if err := scanner.Err(); err != nil {
			h.client.markProcessExited(fmt.Errorf("%w: %w", errCodexProcessExited, err))
		} else {
			h.client.markProcessExited(errCodexProcessExited)
		}
		h.poison.Store(true)
	}()

	initializeCtx, initializeCancel := context.WithCancel(startupCtx)
	defer initializeCancel()
	_, err = h.client.request(initializeCtx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "multica-agent-sdk", "title": "Multica Agent SDK", "version": "0.2.0"},
		"capabilities": map[string]any{"experimentalApi": true},
	})
	if err != nil {
		h.poison.Store(true)
		_ = h.Close(context.Background())
		return nil, fmt.Errorf("codex initialize failed: %w", err)
	}
	h.client.notify("initialized")
	h.baselineProcesses, err = snapshotCodexWarmProcessGroup(startupCtx, cmd)
	if err != nil {
		h.poison.Store(true)
		_ = h.Close(context.Background())
		return nil, fmt.Errorf("snapshot codex warm process group: %w", err)
	}
	cfg.Logger.Info("codex warm lifecycle", "phase", "ready", "pid", cmd.Process.Pid, "runtime_id", cfg.RuntimeID)
	return h, nil
}

func codexWarmSanitizedEnvironment(env map[string]string) map[string]string {
	safe := make(map[string]string, len(env))
	for key, value := range env {
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "MULTICA_") || upper == "TMPDIR" || upper == "TMP" || upper == "TEMP" {
			continue
		}
		safe[key] = value
	}
	return safe
}

func (h *codexWarmHost) Healthy() bool {
	return h != nil && !h.closed.Load() && !h.poison.Load() && h.client.getProcessErr() == nil
}

func (h *codexWarmHost) PrepareIdle(ctx context.Context) error {
	if !h.Healthy() {
		return errors.New("codex warm host is not healthy")
	}
	if err := cleanCodexWarmProcessGroup(ctx, h.cmd, h.baselineProcesses); err != nil {
		h.poison.Store(true)
		return err
	}
	return nil
}

func (h *codexWarmHost) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	if !h.Healthy() {
		return nil, errors.New("codex warm host is not healthy")
	}
	t := &codexWarmTurn{
		messages: make(chan Message, 256), done: make(chan bool, 1),
		activity: make(chan string, 256), gate: &codexTurnNotificationGate{requireStarted: true},
	}
	if !h.active.CompareAndSwap(nil, t) {
		return nil, errors.New("codex warm host already has an active turn")
	}
	results := make(chan Result, 1)
	go h.runTurn(ctx, t, results, prompt, opts)
	return &Session{Messages: t.messages, Result: results}, nil
}

func (h *codexWarmHost) runTurn(ctx context.Context, t *codexWarmTurn, results chan Result, prompt string, opts ExecOptions) {
	defer close(results)
	start := time.Now()
	runCtx, cancel := runContext(ctx, opts.Timeout)
	defer cancel()

	c := h.client
	// The stdout reader outlives turns. Reset its state under the same mutex
	// used by notification dispatch so a late event cannot race the next task's
	// attribution, IDs, completion map, or usage accumulator.
	c.resetTurnState(opts.CodexShellEnv["MULTICA_TASK_ID"])

	status, errText, threadID := "completed", "", ""
	resumed := false
	threadID, resumed, err := c.startOrResumeThread(runCtx, opts, h.cfg.Logger)
	if err != nil {
		status, errText = "failed", err.Error()
		if runCtx.Err() != nil {
			h.poison.Store(true)
			if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				status, errText = "timeout", fmt.Sprintf("codex timed out after %s", opts.Timeout)
			} else {
				status, errText = "aborted", "execution cancelled"
			}
		} else if isCodexTransportError(err) {
			h.poison.Store(true)
		}
		h.finishTurn(t, results, Result{Status: status, Error: errText, DurationMs: time.Since(start).Milliseconds(), ResumeRejected: isCodexResumeOverflow(opts, err)})
		return
	}
	c.setThreadID(threadID)
	turnParams := map[string]any{
		"threadId": threadID,
		"input":    codexTurnInput(prompt, opts.ResumeExpected, resumed, opts.ResumeContinuityNotice),
	}
	applyCodexReasoningEffort(turnParams, opts.ThinkingLevel)
	applyCodexServiceTier(turnParams, opts.ServiceTier)
	t.gate.arm()
	if _, err = c.request(runCtx, "turn/start", turnParams); err != nil {
		select {
		case aborted := <-t.done:
			if aborted {
				status, errText = "aborted", "turn was aborted"
			}
		default:
			status, errText = "failed", fmt.Sprintf("codex turn/start failed: %v", err)
			if runCtx.Err() != nil {
				h.poison.Store(true)
				if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
					status, errText = "timeout", fmt.Sprintf("codex timed out after %s", opts.Timeout)
				} else {
					status, errText = "aborted", "execution cancelled"
				}
			} else if isCodexTransportError(err) {
				h.poison.Store(true)
			}
			h.finishTurn(t, results, Result{Status: status, Error: errText, SessionID: threadID, DurationMs: time.Since(start).Milliseconds()})
			return
		}
	}

	semanticTimeout := opts.SemanticInactivityTimeout
	if semanticTimeout <= 0 {
		semanticTimeout = defaultCodexSemanticInactivityTimeout
	}
	semanticTimer := time.NewTimer(semanticTimeout)
	defer semanticTimer.Stop()
	firstTimeout := codexFirstTurnNoProgressTimeout(semanticTimeout, opts.FirstTurnNoProgressTimeout)
	var firstTimer *time.Timer
	var firstTimerC <-chan time.Time
	firstStarted, firstProgress := false, false
	warmStartupRetryCandidate := false
	stopFirst := func() {
		if firstTimer != nil {
			stopTimer(firstTimer)
			firstTimerC = nil
		}
	}
	defer stopFirst()
	waiting := true
	for waiting {
		select {
		case aborted := <-t.done:
			waiting = false
			if aborted {
				status, errText = "aborted", "turn was aborted"
			}
			if e := c.getTurnError(); e != "" {
				status, errText = "failed", e
			}
		case activity := <-t.activity:
			resetTimer(semanticTimer, semanticTimeout)
			if activity == "status:running" && !firstStarted {
				firstStarted = true
				firstTimer = time.NewTimer(firstTimeout)
				firstTimerC = firstTimer.C
			} else if firstStarted && !firstProgress && isCodexFirstTurnProgressActivity(activity) {
				firstProgress = true
				stopFirst()
			}
		case <-firstTimerC:
			waiting = false
			status = "timeout"
			errText = fmt.Sprintf("%s after %s", CodexFirstTurnNoProgressMarker, firstTimeout)
			warmStartupRetryCandidate = !firstProgress && codexInitializeRetrySupported() && strings.Contains(h.stderr.Tail(), codexModelCatalogRefreshFailureSignal)
			h.poison.Store(true)
		case <-semanticTimer.C:
			waiting = false
			status = "timeout"
			errText = fmt.Sprintf("%s after %s", CodexSemanticInactivityMarker, semanticTimeout)
			h.poison.Store(true)
		case <-runCtx.Done():
			waiting = false
			h.poison.Store(true)
			if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				status, errText = "timeout", fmt.Sprintf("codex timed out after %s", opts.Timeout)
			} else {
				status, errText = "aborted", "execution cancelled"
			}
		case <-c.processDone:
			select {
			case aborted := <-t.done:
				waiting = false
				if aborted {
					status, errText = "aborted", "turn was aborted"
				}
				if e := c.getTurnError(); e != "" {
					status, errText = "failed", e
				}
			default:
				waiting = false
				status = "failed"
				h.poison.Store(true)
				errText = c.getProcessErr().Error()
			}
		}
	}
	_, turnID := c.turnIDs()
	if h.poison.Load() && turnID != "" {
		interruptCtx, interruptCancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, _ = c.request(interruptCtx, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID})
		interruptCancel()
	}

	c.usageMu.Lock()
	usage := c.usage
	c.usageMu.Unlock()
	if usage.InputTokens == 0 && usage.OutputTokens == 0 {
		if scanned := scanCodexSessionUsage(start, strings.TrimSpace(h.cfg.Env["CODEX_HOME"]), threadID, resumed); scanned != nil {
			usage = scanned.usage
			if opts.Model == "" {
				opts.Model = scanned.model
			}
		}
	}
	var usageMap map[string]TokenUsage
	if usage.InputTokens != 0 || usage.OutputTokens != 0 || usage.CacheReadTokens != 0 || usage.CacheWriteTokens != 0 {
		model := opts.Model
		if model == "" {
			model = "unknown"
		}
		usageMap = map[string]TokenUsage{model: usage}
	}
	finalAnswer, lastMessage := t.finish()
	h.active.CompareAndSwap(t, nil)
	results <- Result{
		Status: status, Output: codexDeliverableOutput(finalAnswer, lastMessage), Error: errText,
		SessionID: threadID, DurationMs: time.Since(start).Milliseconds(), Usage: usageMap,
		codexWarmStartupRetryCandidate: warmStartupRetryCandidate,
	}
}

func (h *codexWarmHost) finishTurn(t *codexWarmTurn, results chan Result, result Result) {
	finalAnswer, lastMessage := t.finish()
	if result.Output == "" {
		result.Output = codexDeliverableOutput(finalAnswer, lastMessage)
	}
	h.active.CompareAndSwap(t, nil)
	results <- result
}

func (h *codexWarmHost) Close(ctx context.Context) error {
	if h == nil {
		return nil
	}
	h.closeOnce.Do(func() {
		go h.closeProcess()
	})
	select {
	case <-h.waitDone:
		return h.cleanupErr
	case <-ctx.Done():
		return fmt.Errorf("close codex warm host: %w", context.Cause(ctx))
	}
}

func (h *codexWarmHost) closeProcess() {
	defer close(h.waitDone)
	grace := codexGracefulShutdown()
	h.closed.Store(true)
	h.poison.Store(true)
	_ = h.stdin.Close()
	select {
	case <-h.readerDone:
	case <-time.After(grace):
		h.cancel()
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- h.cmd.Wait() }()
	select {
	case h.waitErr = <-waitCh:
	case <-time.After(grace):
		// The stdout scanner may have stopped on an oversized frame while the
		// child remains blocked writing. Force the owned process tree down, but
		// do not wait forever before quarantining its strict-capacity slot.
		h.cancel()
		select {
		case h.waitErr = <-waitCh:
		case <-time.After(grace):
			h.cleanupErr = errors.New("codex warm host process wait could not be confirmed")
		}
	}
	h.cancel()
	if h.cleanupErr == nil && (h.cmd.ProcessState == nil || !waitProcessGroupGone(h.cmd, grace)) {
		h.cleanupErr = errors.New("codex warm host process-tree cleanup could not be confirmed")
	}
	if h.cleanupErr == nil {
		releaseProcessGroup(h.cmd)
		activeCodexLaunches.Add(-1)
	}
	h.cfg.Logger.Info("codex warm lifecycle", "phase", "closed", "pid", h.cmd.Process.Pid,
		"wait_error", h.waitErr, "cleanup_error", h.cleanupErr)
}
