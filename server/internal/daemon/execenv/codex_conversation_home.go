package execenv

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const codexConversationHomesRoot = "multica-homes-v1"

var errCodexConversationBusy = errors.New("Codex conversation is already running")

// CodexConversationHomeParams deliberately separates execution identity from
// conversation identity. Fresh sessions get a new generation; resumes resolve
// a binding written only after the provider has produced a durable rollout.
type CodexConversationHomeParams struct {
	Profile, WorkspaceID, TaskID, ResumeSessionID string
	Task                                          TaskContextForEnv
}

// CodexConversationHomeLease belongs to the DAEMON, never its preparation helper.
// Keep it until the provider and its tools have exited and results are drained.
type CodexConversationHomeLease struct {
	mu                     sync.Mutex // status pinning may bind concurrently with terminal cleanup
	root, home, generation string
	lock                   *os.File
}

func codexStateDigest(parts ...string) string {
	data, _ := json.Marshal(parts)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validCodexStateDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && hex.EncodeToString(b) == s
}

func codexConversationIdentity(p CodexConversationHomeParams) string {
	kind, id := "issue", p.Task.IssueID
	if id == "" {
		kind, id = "chat", p.Task.ChatSessionID
	}
	if p.WorkspaceID == "" || p.Task.AgentID == "" || id == "" || p.TaskID == "" {
		return ""
	}
	return codexStateDigest(p.WorkspaceID, p.Task.AgentID, kind, id)
}

// privateCodexChild accepts only an actual directory, not a link into another
// conversation. Parents are constructed a component at a time from a canonical
// shared home. This is isolation between normal executions, not a sandbox for
// hostile processes running as the same OS user.
func privateCodexChild(parent, name string) (string, error) {
	dir := filepath.Join(parent, name)
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return "", err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("Codex state directory is not a real directory: %s", dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func codexConversationNamespace(profile string, create bool) (string, error) {
	shared := resolveSharedCodexHome()
	if create {
		if err := os.MkdirAll(shared, 0o700); err != nil {
			return "", err
		}
	}
	shared, err := filepath.EvalSymlinks(shared)
	if err != nil {
		return "", err
	}
	if !create {
		return filepath.Join(shared, codexConversationHomesRoot, codexSessionStoreNamespace(profile)), nil
	}
	root, err := privateCodexChild(shared, codexConversationHomesRoot)
	if err != nil {
		return "", err
	}
	return privateCodexChild(root, codexSessionStoreNamespace(profile))
}

func lockCodexConversation(root string) (*os.File, error) {
	path := filepath.Join(root, "lease.lock")
	fi, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil && !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("invalid Codex lease file: %s", path)
	}
	f, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	opened, statErr := f.Stat()
	actual, pathErr := os.Lstat(path)
	if statErr != nil || pathErr != nil || !actual.Mode().IsRegular() || !os.SameFile(opened, actual) {
		f.Close()
		return nil, fmt.Errorf("Codex lease file changed: %s", path)
	}
	ok, err := lockFileExclusiveNonBlocking(f)
	if err != nil || !ok {
		f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s", errCodexConversationBusy, root)
	}
	return f, nil
}

// ClaimCodexConversationHome enrolls new conversations without migrating old
// databases. A resume with no binding is legacy and returns nil. An existing
// binding with missing/corrupt state is an error, never a legacy fallback.
func ClaimCodexConversationHome(p CodexConversationHomeParams) (*CodexConversationHomeLease, error) {
	identity := codexConversationIdentity(p)
	if identity == "" {
		return nil, nil
	}
	namespace, err := codexConversationNamespace(p.Profile, true)
	if err != nil {
		return nil, err
	}
	root, err := privateCodexChild(namespace, identity)
	if err != nil {
		return nil, err
	}
	lock, err := lockCodexConversation(root)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			releaseLockFile(lock)
		}
	}()
	if _, err := os.Lstat(filepath.Join(root, "active.json")); !os.IsNotExist(err) {
		return nil, fmt.Errorf("Codex conversation has an unclean execution; verify provider processes have exited before recovery: %s", root)
	}
	bindings, err := privateCodexChild(root, "bindings")
	if err != nil {
		return nil, err
	}
	generation := codexStateDigest(p.TaskID)
	if p.ResumeSessionID != "" {
		path := filepath.Join(bindings, codexStateDigest(p.ResumeSessionID)+".json")
		fi, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("invalid Codex session binding: %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var binding struct {
			Generation string `json:"generation"`
		}
		if err := json.Unmarshal(data, &binding); err != nil || !validCodexStateDigest(binding.Generation) {
			return nil, fmt.Errorf("invalid Codex session binding: %s", path)
		}
		generation = binding.Generation
	}
	gens, err := privateCodexChild(root, "generations")
	if err != nil {
		return nil, err
	}
	home := filepath.Join(gens, generation)
	if p.ResumeSessionID != "" {
		fi, err := os.Lstat(home)
		if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("persisted Codex home unavailable; restore its cold backup or explicitly start a fresh session: %s", home)
		}
	}
	home, err = privateCodexChild(gens, generation)
	if err != nil {
		return nil, err
	}
	active, _ := json.Marshal(struct {
		TaskID string `json:"task_id"`
		PID    int    `json:"daemon_pid"`
	}{p.TaskID, os.Getpid()})
	if err := writeFileAtomic(filepath.Join(root, "active.json"), active, 0o600); err != nil {
		return nil, err
	}
	keep = true
	return &CodexConversationHomeLease{root: root, home: home, generation: generation, lock: lock}, nil
}

func (l *CodexConversationHomeLease) Home() string {
	if l == nil {
		return ""
	}
	return l.home
}

func validateLeasedCodexHome(home string) error {
	canonical, err := filepath.EvalSymlinks(home)
	if err != nil || canonical != home || !validCodexStateDigest(filepath.Base(home)) || filepath.Base(filepath.Dir(home)) != "generations" {
		return fmt.Errorf("invalid leased Codex home: %s", home)
	}
	root := filepath.Dir(filepath.Dir(home))
	fi, err := os.Lstat(filepath.Join(root, "active.json"))
	if err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("Codex home has no execution lease: %s", home)
	}
	// The helper must not acquire/release the parent's lease. Verify that the
	// lock is held before touching configuration; a marker alone is insufficient.
	lock, err := lockCodexConversation(root)
	if err == nil {
		releaseLockFile(lock)
		return fmt.Errorf("Codex home execution lock is not held: %s", home)
	}
	if !errors.Is(err, errCodexConversationBusy) {
		return err
	}
	return nil
}

func (l *CodexConversationHomeLease) BindSession(sessionID string) error {
	if l == nil {
		return errors.New("no Codex home lease")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lock == nil || sessionID == "" {
		return errors.New("cannot bind Codex session without an active home lease")
	}
	path := filepath.Join(l.root, "bindings", codexStateDigest(sessionID)+".json")
	data, _ := json.Marshal(struct {
		Generation string `json:"generation"`
	}{l.generation})
	if prior, err := os.ReadFile(path); err == nil && string(prior) != string(data) {
		return errors.New("Codex session is already bound to a different home generation")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return writeFileAtomic(path, data, 0o600)
}

// Release is for orderly completion only. A dead daemon leaves active.json:
// losing an advisory lock does not prove orphaned provider children are gone.
func (l *CodexConversationHomeLease) Release() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lock == nil {
		return
	}
	now := time.Now()
	_ = os.Chtimes(l.home, now, now)
	_ = os.Remove(filepath.Join(l.root, "active.json"))
	releaseLockFile(l.lock)
	l.lock = nil
}

// Quarantine releases the OS lock but deliberately retains active.json. Use
// when execution started but its process-tree cleanup could not be proven.
func (l *CodexConversationHomeLease) Quarantine() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lock == nil {
		return
	}
	releaseLockFile(l.lock)
	l.lock = nil
}

// PruneCodexConversationHomes shares the execution lock, never follows links,
// and never deletes the lock inode. Small bindings remain as tombstones so GC
// loss cannot masquerade as a pre-upgrade session eligible for legacy fallback.
func PruneCodexConversationHomes(profile string, retention time.Duration, now time.Time, logger *slog.Logger) (removed int, bytesFreed int64) {
	if retention <= 0 {
		return
	}
	namespace, err := codexConversationNamespace(profile, false)
	if err != nil {
		return
	}
	// Reject linked state roots on the read-only GC path too.
	for _, path := range []string{filepath.Dir(namespace), namespace} {
		fi, err := os.Lstat(path)
		if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return
		}
	}
	entries, err := os.ReadDir(namespace)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validCodexStateDigest(entry.Name()) {
			continue
		}
		root := filepath.Join(namespace, entry.Name())
		lock, err := lockCodexConversation(root)
		if err != nil {
			continue
		}
		func() {
			defer releaseLockFile(lock)
			if _, err := os.Lstat(filepath.Join(root, "active.json")); !os.IsNotExist(err) {
				return
			}
			gens := filepath.Join(root, "generations")
			fi, err := os.Lstat(gens)
			if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
				return
			}
			homes, err := os.ReadDir(gens)
			if err != nil {
				return
			}
			for _, home := range homes {
				if !home.IsDir() || !validCodexStateDigest(home.Name()) {
					continue
				}
				path := filepath.Join(gens, home.Name())
				newest, size := dirStat(path)
				if newest.IsZero() || now.Sub(newest) <= retention {
					continue
				}
				if err := os.RemoveAll(path); err != nil {
					logger.Warn("prune Codex home failed", "error", err)
					continue
				}
				removed++
				bytesFreed += size
			}
		}()
	}
	return
}
