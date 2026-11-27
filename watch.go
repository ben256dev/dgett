package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
)

const (
	watchRunDir  = "/run/dgett/watch"
	watchDataDir = "/var/lib/dgett/watch"
	cacheDir     = "/var/lib/dgett/cache"
)

// gen is one running generation of a watched app.
type gen struct {
	pid  int
	done chan struct{}
	sock string
}

type watcher struct {
	path   string
	branch string
	config string
	cfg    Config
	app    string
	url    string
	uid    int
	cred   *syscall.Credential
	home   string
	name   string
	data     string
	cache    string
	report   string
	last     string
	quit     chan struct{}
	gens     int
	draining []*gen
	hub      hub
	// lastRebuild is how long the last successful rebuild took, from change
	// detected to swap; sent with "building" as the page's estimate.
	lastRebuild time.Duration
}

func watched(patterns []string, branch string) bool {
	if branch == "" {
		return false
	}
	for _, p := range patterns {
		if ok, _ := path.Match(p, branch); ok {
			return true
		}
	}
	return false
}

func peerUID(conn net.Conn) (int, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *syscall.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if cerr != nil {
		return 0, cerr
	}
	return int(cred.Uid), nil
}

func (w *watcher) closed() bool {
	select {
	case <-w.quit:
		return true
	default:
		return false
	}
}

func (w *watcher) env() []string {
	return append(os.Environ(), "HOME="+w.home, "USER="+w.name, "LOGNAME="+w.name)
}

func (w *watcher) git(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", w.path}, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: w.cred}
	cmd.Env = w.env()
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// sig fingerprints every tracked or unignored file plus .env, so any edit,
// add or delete changes it.
func (w *watcher) sig() string {
	out, err := w.git("ls-files", "-co", "--exclude-standard", "-z")
	if err != nil {
		return ""
	}
	files := strings.Split(out, "\x00")
	sort.Strings(files)
	h := sha256.New()
	for _, f := range files {
		if f == "" {
			continue
		}
		fmt.Fprint(h, f)
		if fi, err := os.Lstat(filepath.Join(w.path, f)); err == nil {
			fmt.Fprintf(h, " %d %d\n", fi.Size(), fi.ModTime().UnixNano())
		} else {
			fmt.Fprint(h, " -\n")
		}
	}
	if fi, err := os.Stat(filepath.Join(envDir, w.app+".env")); err == nil {
		fmt.Fprintf(h, "env %d %d\n", fi.Size(), fi.ModTime().UnixNano())
	}
	return hex.EncodeToString(h.Sum(nil))
}

// build runs build on the first start and rebuild (falling back to build)
// on later restarts.
func (w *watcher) build(id string, rebuild bool) ([]byte, error) {
	command := w.cfg.Build
	if rebuild && w.cfg.Rebuild != "" {
		command = w.cfg.Rebuild
	}
	if command == "" {
		return nil, nil
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = w.path
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: w.cred}
	cmd.Env = append(w.env(), "DGETT_BUILD=1", "DGETT_CACHE="+w.cache)
	t := time.Now()
	out, err := cmd.CombinedOutput()
	name := "build"
	if rebuild && w.cfg.Rebuild != "" {
		name = "rebuild"
	}
	w.logf(id, "%s %.1fs", name, time.Since(t).Seconds())
	return out, err
}

func (w *watcher) logf(id, format string, args ...any) {
	f, err := os.OpenFile(filepath.Join(logsDir, id+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "dgett: "+format+"\n", args...)
}

// launch starts a new generation of a watched app on its own socket. The
// current generation keeps running until the caller swaps it out.
func (w *watcher) launch(id, envFile string) (*gen, int64, error) {
	w.gens++
	sock := filepath.Join(watchRunDir, id, fmt.Sprintf("gen-%d.sock", w.gens))
	os.Remove(sock)
	os.Remove(w.report)
	os.MkdirAll(logsDir, 0o755)
	logf, err := os.OpenFile(filepath.Join(logsDir, id+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, 0, err
	}
	logStart, _ := logf.Seek(0, 2)

	if _, _, err := appDirs(w.app); err != nil {
		logf.Close()
		return nil, 0, err
	}
	vars, err := godotenv.Read(envFile)
	if err != nil {
		logf.Close()
		return nil, 0, fmt.Errorf("env file %s: %v", envFile, err)
	}
	env := w.env()
	for k, v := range vars {
		env = append(env, k+"="+v)
	}

	run := exec.Command("sh", "-c", w.cfg.Run)
	run.Dir = w.path
	run.Env = append(env, "DGETT_SOCKET="+sock, "DGETT_DATA="+w.data, "DGETT_CACHE="+w.cache, "DGETT_ENV="+os.DevNull, "DGETT_REPORT="+w.report, "DGETT_URL="+w.url)
	run.Stdout, run.Stderr = logf, logf
	run.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: w.cred}
	if err := run.Start(); err != nil {
		logf.Close()
		return nil, 0, err
	}
	g := &gen{pid: run.Process.Pid, done: make(chan struct{}), sock: sock}
	go func() {
		run.Wait()
		logf.Close()
		close(g.done)
	}()
	return g, logStart, nil
}

// drain stops a generation that has been swapped out, letting it finish its
// in-flight requests first.
func (w *watcher) drain(g *gen) {
	killGroup(g.pid, g.done)
	os.Remove(g.sock)
	mu.Lock()
	for i, x := range w.draining {
		if x == g {
			w.draining = append(w.draining[:i], w.draining[i+1:]...)
			break
		}
	}
	mu.Unlock()
}

func newWatcher(req Request, cfg Config, uid int, app, url, id string) (*watcher, error) {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return nil, err
	}
	gid, _ := strconv.Atoi(u.Gid)
	var groups []uint32
	if ids, err := u.GroupIds(); err == nil {
		for _, g := range ids {
			if n, err := strconv.Atoi(g); err == nil {
				groups = append(groups, uint32(n))
			}
		}
	}
	if fi, err := os.Stat(req.Path); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("worktree %s not found", req.Path)
	}

	sockDir := filepath.Join(watchRunDir, id)
	if err := os.MkdirAll(watchRunDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(sockDir, 0o750); err != nil {
		return nil, err
	}
	if err := os.Chown(sockDir, uid, int(appCred.Gid)); err != nil {
		return nil, err
	}
	if err := os.Chmod(sockDir, 0o750|os.ModeSetgid); err != nil {
		return nil, err
	}
	data := filepath.Join(watchDataDir, id)
	if err := os.MkdirAll(watchDataDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(data, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chown(data, uid, gid); err != nil {
		return nil, err
	}
	cache := filepath.Join(cacheDir, id)
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chown(cache, uid, gid); err != nil {
		return nil, err
	}

	return &watcher{
		path:   req.Path,
		branch: req.Branch,
		config: req.Config,
		cfg:    cfg,
		app:    app,
		url:    url,
		uid:    uid,
		cred:   &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups},
		home:   u.HomeDir,
		name:   u.Username,
		data:   data,
		cache:  cache,
		report: filepath.Join(sockDir, "report"),
		quit:   make(chan struct{}),
	}, nil
}

func watchDeploy(req Request, cfg Config, uid int) Response {
	if !watched(cfg.Watch, req.Branch) {
		return Response{Error: fmt.Sprintf("branch %q is not in watch", req.Branch)}
	}
	r := req
	r.Dirty = false
	p, err := planRoutes(r, cfg)
	if err != nil {
		return Response{Error: err.Error()}
	}
	id := p.app + "--" + label(req.Branch)

	mu.Lock()
	old := deploys[id]
	running := false
	if old != nil {
		select {
		case <-old.done:
		default:
			running = true
		}
	}
	mu.Unlock()
	if old != nil && old.w != nil && running && old.w.path == req.Path && old.w.config == req.Config && old.w.uid == uid {
		mu.Lock()
		hosts := hostsOf(id)
		mu.Unlock()
		sort.Strings(hosts)
		var urls []string
		for _, h := range hosts {
			urls = append(urls, "https://"+h)
		}
		return Response{OK: true, URLs: urls}
	}
	if old != nil {
		stop(id)
	}

	w, err := newWatcher(req, cfg, uid, p.app, "https://"+p.hosts[0], id)
	if err != nil {
		return Response{Error: "watch: " + err.Error()}
	}
	if out, err := w.build(id, false); err != nil {
		unroute(id)
		return Response{Error: "build failed:\n" + string(out)}
	}
	w.last = w.sig()

	envFile := filepath.Join(envDir, p.app+".env")
	g, logStart, err := w.launch(id, envFile)
	if err != nil {
		unroute(id)
		return Response{Error: "start failed: " + err.Error()}
	}
	d := &Deploy{
		Commit:  id,
		PID:     g.pid,
		Started: time.Now(),
		Watch:   true,
		Branch:  req.Branch,
		Path:    req.Path,
		Socket:  g.sock,
		EnvFile: envFile,
		done:    g.done,
		w:       w,
	}
	mu.Lock()
	deploys[id] = d
	mu.Unlock()

	if err := waitSocket(d, time.Minute); err != nil {
		tail := logTail(id, logStart, 20)
		unroute(id)
		stop(id)
		if r, ok := readReport(w.report, p.app); ok && len(r.MissingEnv) > 0 {
			r.EnvFile = d.EnvFile
			return missingEnvError(r, p.app)
		}
		return Response{Error: "start failed: " + err.Error() + "\n" + tail}
	}
	readReport(w.report, p.app)

	urls, err := applyRoutes(d, p)
	if err != nil {
		stop(id)
		return Response{Error: err.Error()}
	}
	go w.loop(d)
	return Response{OK: true, URLs: urls}
}

func (w *watcher) loop(d *Deploy) {
	id := d.Commit
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-w.quit:
			return
		case <-t.C:
		}

		if br, err := w.git("symbolic-ref", "--short", "-q", "HEAD"); err != nil || br != w.branch {
			opMu.Lock()
			if !w.closed() {
				w.logf(id, "worktree %s left branch %s, stopping", w.path, w.branch)
				unroute(id)
				stop(id)
			}
			opMu.Unlock()
			return
		}

		if w.sig() == w.last {
			continue
		}
		time.Sleep(300 * time.Millisecond)
		w.last = w.sig()
		w.logf(id, "change detected, rebuilding")
		started := time.Now()
		w.hub.send("building", fmt.Sprintf(`{"started":%d,"estimate":%d}`, started.UnixMilli(), w.lastRebuild.Milliseconds()))
		w.restart(d, started)
	}
}

// restart builds and starts a new generation while the current one keeps
// serving, then swaps traffic over and drains the old one.
func (w *watcher) restart(d *Deploy, started time.Time) {
	id := d.Commit
	out, err := w.build(id, true)
	w.last = w.sig()
	if err != nil {
		w.logf(id, "build failed, keeping previous version:\n%s", out)
		w.hub.send("builderror", "build failed:\n"+string(out))
		return
	}

	g, logStart, err := w.launch(id, d.EnvFile)
	if err != nil {
		w.logf(id, "start failed: %v", err)
		w.hub.send("builderror", "start failed: "+err.Error())
		return
	}
	if err := waitSocket(&Deploy{Socket: g.sock, done: g.done}, time.Minute); err != nil {
		tail := logTail(id, logStart, 20)
		killGroup(g.pid, g.done)
		os.Remove(g.sock)
		msg := "start failed: " + err.Error() + "\n" + tail
		if r, ok := readReport(w.report, w.app); ok && len(r.MissingEnv) > 0 {
			msg = "missing env keys in " + d.EnvFile + ": " + strings.Join(r.MissingEnv, ", ")
		}
		w.logf(id, "%s, keeping previous version", msg)
		w.hub.send("builderror", msg)
		return
	}
	readReport(w.report, w.app)

	opMu.Lock()
	if w.closed() {
		opMu.Unlock()
		killGroup(g.pid, g.done)
		os.Remove(g.sock)
		return
	}
	mu.Lock()
	old := &gen{pid: d.PID, done: d.done, sock: d.Socket}
	d.PID, d.done, d.Socket, d.Started = g.pid, g.done, g.sock, time.Now()
	w.draining = append(w.draining, old)
	mu.Unlock()
	opMu.Unlock()

	w.lastRebuild = time.Since(started)
	w.logf(id, "swapped to %s", filepath.Base(g.sock))
	w.hub.send("reload", "")
	go w.drain(old)
}
