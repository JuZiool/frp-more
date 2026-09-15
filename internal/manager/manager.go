// Package manager maintains a set of frpc client instances, each driven by one
// TOML config file under <dataDir>/instances/. Instances run in-process by
// embedding github.com/fatedier/frp/client, mirroring what cmd/frpc does.
package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fatedier/frp/client"
	"github.com/fatedier/frp/client/proxy"
	"github.com/fatedier/frp/pkg/config"
	"github.com/fatedier/frp/pkg/config/source"
	v1 "github.com/fatedier/frp/pkg/config/v1"
	"github.com/fatedier/frp/pkg/config/v1/validation"
	"github.com/fatedier/frp/pkg/policy/security"
	"github.com/fatedier/frp/pkg/util/log"
	"github.com/fatedier/frp/pkg/util/xlog"

	"frp-more/internal/logbuf"
)

const (
	StateRunning = "running"
	StateStopped = "stopped"
	StateExited  = "exited"
)

// nameRegexp allows Unicode letters (incl. Chinese) and digits, plus space and
// . _ - . File-safety checks (separators, reserved names, dots) live in ValidName.
var nameRegexp = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._-]*$`)

func ValidName(name string) bool {
	n := len([]rune(name))
	if n == 0 || n > 64 {
		return false
	}
	if name != strings.TrimSpace(name) {
		return false
	}
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
		return false
	}
	if !nameRegexp.MatchString(name) {
		return false
	}
	switch strings.ToUpper(name) {
	case "CON", "PRN", "AUX", "NUL":
		return false
	}
	for i := 1; i <= 9; i++ {
		if strings.EqualFold(name, "COM"+strconv.Itoa(i)) ||
			strings.EqualFold(name, "LPT"+strconv.Itoa(i)) {
			return false
		}
	}
	return true
}

// ProxyInfo is the UI-facing status of one proxy of an instance.
type ProxyInfo struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Phase      string `json:"phase"`
	RemoteAddr string `json:"remoteAddr"`
	Err        string `json:"err,omitempty"`
}

type VisitorInfo struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// Info is the UI-facing snapshot of one instance.
type Info struct {
	Name        string        `json:"name"`
	State       string        `json:"state"`
	LastErr     string        `json:"lastErr,omitempty"`
	StartedAt   string        `json:"startedAt,omitempty"`
	AutoStart   bool          `json:"autoStart"`
	Proxies     []ProxyInfo   `json:"proxies"`
	Visitors    []VisitorInfo `json:"visitors,omitempty"`
	ProxyOnline int           `json:"proxyOnline"`
}

type Instance struct {
	Name    string
	cfgPath string
	unsafe  *security.UnsafeFeatures
	logs    *logbuf.InstanceStore

	mu          sync.Mutex
	cancel      context.CancelFunc
	svr         *client.Service
	done        chan struct{}
	state       string
	lastErr     string
	startedAt   time.Time
	common      *v1.ClientCommonConfig
	proxyCfgs   []v1.ProxyConfigurer
	visitorCfgs []v1.VisitorConfigurer
}

func newInstance(name, cfgPath string, unsafe *security.UnsafeFeatures, stores ...*logbuf.InstanceStore) *Instance {
	var logs *logbuf.InstanceStore
	if len(stores) > 0 {
		logs = stores[0]
	}
	if logs != nil {
		logs.Register(name)
	}
	return &Instance{
		Name:    name,
		cfgPath: cfgPath,
		unsafe:  unsafe,
		logs:    logs,
		state:   StateStopped,
	}
}

// instanceLogger adds a stable routing tag to all lifecycle and FRP service
// messages emitted for this instance.
func (i *Instance) instanceLogger() *xlog.Logger {
	return xlog.New().AddPrefix(xlog.LogPrefix{
		Name:     "frp-more-instance",
		Value:    logbuf.InstanceTag(i.Name),
		Priority: 1,
	})
}

// buildAggregator parses the instance config file the same way cmd/frpc does:
// load -> seed config source -> aggregate -> filter/complete -> validate.
// The returned aggregator is the one to hand to client.NewService.
func (i *Instance) buildAggregator() (*v1.ClientCommonConfig, *source.Aggregator, []v1.ProxyConfigurer, []v1.VisitorConfigurer, error) {
	result, err := config.LoadClientConfigResult(i.cfgPath, true)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	configSource := source.NewConfigSource()
	if err := configSource.ReplaceAll(result.Proxies, result.Visitors); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("set config source: %w", err)
	}

	aggregator := source.NewAggregator(configSource)
	if result.Common.Store.IsEnabled() {
		storePath := result.Common.Store.Path
		if storePath != "" && !filepath.IsAbs(storePath) {
			storePath = filepath.Join(filepath.Dir(i.cfgPath), storePath)
		}
		storeSource, err := source.NewStoreSource(source.StoreSourceConfig{Path: storePath})
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("create store source: %w", err)
		}
		aggregator.SetStoreSource(storeSource)
	}

	proxyCfgs, visitorCfgs, err := aggregator.Load()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("load config: %w", err)
	}

	proxyCfgs, visitorCfgs = config.FilterClientConfigurers(result.Common, proxyCfgs, visitorCfgs)
	proxyCfgs = config.CompleteProxyConfigurers(proxyCfgs)
	visitorCfgs = config.CompleteVisitorConfigurers(visitorCfgs)

	warning, err := validation.ValidateAllClientConfig(result.Common, proxyCfgs, visitorCfgs, i.unsafe)
	if warning != nil {
		i.instanceLogger().Warnf("config warning: %v", warning)
	}
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if i.logs != nil {
		i.logs.Register(i.Name)
	}
	return result.Common, aggregator, proxyCfgs, visitorCfgs, nil
}

// start launches the embedded frpc service in a goroutine.
func (i *Instance) start() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	switch i.state {
	case StateRunning:
		return fmt.Errorf("instance %s is already running", i.Name)
	case StateStopped, StateExited:
	default:
		return fmt.Errorf("instance %s is stopping, try again later", i.Name)
	}

	common, aggregator, proxyCfgs, visitorCfgs, err := i.buildAggregator()
	if err != nil {
		i.instanceLogger().Errorf("start failed: %v", err)
		return err
	}

	svr, err := client.NewService(client.ServiceOptions{
		Common:                 common,
		ConfigSourceAggregator: aggregator,
		UnsafeFeatures:         i.unsafe,
		ConfigFilePath:         i.cfgPath,
	})
	if err != nil {
		i.instanceLogger().Errorf("create frpc service failed: %v", err)
		return err
	}

	instanceLogger := i.instanceLogger()
	ctx, cancel := context.WithCancel(xlog.NewContext(context.Background(), instanceLogger))
	done := make(chan struct{})
	i.cancel = cancel
	i.svr = svr
	i.done = done
	i.common = common
	i.proxyCfgs = proxyCfgs
	i.visitorCfgs = visitorCfgs
	i.state = StateRunning
	i.lastErr = ""
	i.startedAt = time.Now()

	go func() {
		defer close(done)
		err := svr.Run(ctx)
		i.mu.Lock()
		wasRunning := i.state == StateRunning
		if wasRunning { // exited on its own, not a manual stop
			i.state = StateExited
			if err != nil {
				i.lastErr = err.Error()
			}
		}
		i.mu.Unlock()
		if err != nil && wasRunning {
			instanceLogger.Errorf("frpc exited unexpectedly: %v", err)
		} else if err != nil {
			instanceLogger.Debugf("frpc stopped: %v", err)
		}
		i.mu.Lock()
		i.svr = nil
		i.cancel = nil
		i.done = nil
		i.mu.Unlock()
	}()

	instanceLogger.Infof("started (server %s:%d, %d proxies)",
		common.ServerAddr, common.ServerPort, len(proxyCfgs))
	return nil
}

// stop gracefully closes the service and waits (bounded) for Run to return.
func (i *Instance) stop() {
	i.mu.Lock()
	if i.state != StateRunning {
		i.mu.Unlock()
		return
	}
	i.state = StateStopped
	instanceLogger := i.instanceLogger()
	svr, cancel, done := i.svr, i.cancel, i.done
	i.svr, i.cancel, i.done = nil, nil, nil
	i.mu.Unlock()

	if svr != nil {
		svr.GracefulClose(500 * time.Millisecond)
	}
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			instanceLogger.Warnf("did not stop within 3s")
		}
	}
	instanceLogger.Infof("stopped")
}

// loadSnapshot parses the config without starting the service, so that a
// stopped instance can still display its configured proxies.
func (i *Instance) loadSnapshot() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.proxyCfgs != nil {
		return
	}
	common, _, proxyCfgs, visitorCfgs, err := i.buildAggregator()
	if err != nil {
		return
	}
	i.common = common
	i.proxyCfgs = proxyCfgs
	i.visitorCfgs = visitorCfgs
}

// Info snapshots the instance state; a running instance also reports proxy status.
func (i *Instance) Info() Info {
	i.mu.Lock()
	defer i.mu.Unlock()

	info := Info{
		Name:      i.Name,
		State:     i.state,
		LastErr:   i.lastErr,
		AutoStart: true,
		Proxies:   []ProxyInfo{},
	}
	if !i.startedAt.IsZero() && i.state == StateRunning {
		info.StartedAt = i.startedAt.Format(time.RFC3339)
	}

	for _, vc := range i.visitorCfgs {
		base := vc.GetBaseConfig()
		info.Visitors = append(info.Visitors, VisitorInfo{Name: base.Name, Type: base.Type})
	}

	if i.svr == nil {
		// Not running: still show the proxies of the last loaded config, if any.
		for _, pc := range i.proxyCfgs {
			base := pc.GetBaseConfig()
			info.Proxies = append(info.Proxies, ProxyInfo{Name: base.Name, Type: base.Type, Phase: "stopped"})
		}
		return info
	}
	exporter := i.svr.StatusExporter()
	for _, pc := range i.proxyCfgs {
		base := pc.GetBaseConfig()
		p := ProxyInfo{Name: base.Name, Type: base.Type, Phase: "not connected"}
		if ws, ok := exporter.GetProxyStatus(base.Name); ok {
			p.Phase = ws.Phase
			p.RemoteAddr = ws.RemoteAddr
			p.Err = ws.Err
		}
		if p.Phase == proxy.ProxyPhaseRunning {
			info.ProxyOnline++
		}
		info.Proxies = append(info.Proxies, p)
	}
	return info
}

// Manager owns the instance registry and the persisted stopped-state set.
type Manager struct {
	// opMu serializes mutating operations so config replacement and rollback
	// cannot interleave with another lifecycle or file operation.
	opMu      sync.Mutex
	mu        sync.Mutex
	dir       string
	statePath string
	unsafe    *security.UnsafeFeatures
	logs      *logbuf.InstanceStore
	instances map[string]*Instance
	stopped   map[string]bool
}

// SetInstanceLogs attaches the per-instance log store used by future and
// already discovered instances.
func (m *Manager) SetInstanceLogs(logs *logbuf.InstanceStore) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	m.logs = logs
	for _, inst := range m.instances {
		inst.mu.Lock()
		inst.logs = logs
		name := inst.Name
		inst.mu.Unlock()
		if logs != nil {
			logs.Register(name)
		}
	}
	m.mu.Unlock()
}

func NewManager(dataDir string) (*Manager, error) {
	dir := filepath.Join(dataDir, "instances")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	m := &Manager{
		dir:       dir,
		statePath: filepath.Join(dataDir, "state.json"),
		unsafe:    security.NewUnsafeFeatures(nil),
		instances: map[string]*Instance{},
		stopped:   map[string]bool{},
	}
	if err := m.loadState(); err != nil {
		log.Warnf("load state: %v", err)
	}
	return m, nil
}

type persistedState struct {
	Stopped []string `json:"stopped"`
}

func (m *Manager) loadState() error {
	b, err := os.ReadFile(m.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var st persistedState
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	for _, name := range st.Stopped {
		m.stopped[name] = true
	}
	return nil
}

func (m *Manager) saveState() error {
	st := persistedState{Stopped: []string{}}
	for name, stopped := range m.stopped {
		if stopped {
			st.Stopped = append(st.Stopped, name)
		}
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.statePath, b, 0o644)
}

// ScanDir discovers instance config files on disk and registers the new ones,
// auto-starting them unless the persisted state marks them stopped. Instances
// whose file disappeared are removed (after being stopped).
func (m *Manager) ScanDir() {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()

	entries, err := os.ReadDir(m.dir)
	if err != nil {
		log.Errorf("scan instance dir: %v", err)
		return
	}

	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".toml" {
			continue
		}
		name := e.Name()[:len(e.Name())-len(".toml")]
		if !ValidName(name) {
			log.Warnf("skip invalid instance config file name: %s", e.Name())
			continue
		}
		seen[name] = true
		if _, ok := m.instances[name]; ok {
			continue
		}
		inst := newInstance(name, filepath.Join(m.dir, e.Name()), m.unsafe, m.logs)
		m.instances[name] = inst
		if m.stopped[name] {
			log.Infof("instance [%s] discovered, kept stopped by persisted state", name)
			inst.loadSnapshot()
			continue
		}
		if err := inst.start(); err != nil {
			log.Errorf("instance [%s] auto-start failed: %v", name, err)
			inst.mu.Lock()
			inst.state = StateExited
			inst.lastErr = err.Error()
			inst.mu.Unlock()
		}
	}

	for name, inst := range m.instances {
		if !seen[name] {
			inst.stop()
			delete(m.instances, name)
			log.Infof("instance [%s] removed (config file gone)", name)
		}
	}
}

func (m *Manager) get(name string) (*Instance, error) {
	inst, ok := m.instances[name]
	if !ok {
		return nil, fmt.Errorf("instance %s not found", name)
	}
	return inst, nil
}

func (m *Manager) List() []Info {
	m.mu.Lock()
	names := make([]string, 0, len(m.instances))
	for name := range m.instances {
		names = append(names, name)
	}
	m.mu.Unlock()

	sort.Strings(names)
	infos := make([]Info, 0, len(names))
	for _, name := range names {
		m.mu.Lock()
		inst := m.instances[name]
		m.mu.Unlock()
		if inst != nil {
			infos = append(infos, inst.Info())
		}
	}
	return infos
}

func (m *Manager) Start(name string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	inst, err := m.get(name)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if err := inst.start(); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.stopped, name)
	err = m.saveState()
	m.mu.Unlock()
	return err
}

func (m *Manager) Stop(name string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	inst, err := m.get(name)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	inst.stop()
	m.mu.Lock()
	m.stopped[name] = true
	err = m.saveState()
	m.mu.Unlock()
	return err
}

func (m *Manager) Restart(name string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	inst, err := m.get(name)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	inst.stop()
	return inst.start()
}

// Create writes a new instance config (validated first) and starts it.
func (m *Manager) Create(name, content string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if !ValidName(name) {
		return fmt.Errorf("invalid instance name %q: use Chinese, letters, digits, space or . _ -, at most 64 chars", name)
	}
	m.mu.Lock()
	_, exists := m.instances[name]
	dir := m.dir
	m.mu.Unlock()
	if exists {
		return fmt.Errorf("instance %s already exists", name)
	}

	tmpPath := filepath.Join(dir, name+".toml.tmp")
	if err := os.WriteFile(tmpPath, []byte(content), 0o644); err != nil {
		return err
	}
	check := newInstance(name, tmpPath, m.unsafe, m.logs)
	if _, _, _, _, err := check.buildAggregator(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("config invalid: %w", err)
	}

	finalPath := filepath.Join(dir, name+".toml")
	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	inst := newInstance(name, finalPath, m.unsafe, m.logs)

	m.mu.Lock()
	m.instances[name] = inst
	m.mu.Unlock()

	if err := inst.start(); err != nil {
		log.Errorf("instance [%s] created but start failed: %v", name, err)
		inst.mu.Lock()
		inst.state = StateExited
		inst.lastErr = err.Error()
		inst.mu.Unlock()
	}
	m.mu.Lock()
	delete(m.stopped, name)
	saveErr := m.saveState()
	m.mu.Unlock()
	return saveErr
}

func writeTempConfig(dir, base string, content []byte) (string, error) {
	f, err := os.CreateTemp(dir, "."+base+"-*.tmp")
	if err != nil {
		return "", err
	}
	path := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(path)
	}
	if err := f.Chmod(0o644); err != nil {
		cleanup()
		return "", err
	}
	if _, err := f.Write(content); err != nil {
		cleanup()
		return "", err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func replaceFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else {
		// Windows may refuse to rename over an existing file. Only remove the
		// destination after confirming it exists, then retry the replacement.
		if _, statErr := os.Stat(dst); statErr != nil {
			return err
		}
		if removeErr := os.Remove(dst); removeErr != nil {
			return fmt.Errorf("replace destination: %w", removeErr)
		}
		if retryErr := os.Rename(src, dst); retryErr != nil {
			return fmt.Errorf("rename replacement: %w", retryErr)
		}
		return nil
	}
}

// UpdateConfig replaces the instance config file (validated first); a running
// instance is restarted to apply it. When newName differs from the current
// name, the config file is renamed and the instance re-registered as well.
func (m *Manager) UpdateConfig(name, newName, content string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.Lock()
	inst, err := m.get(name)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if newName == "" {
		newName = name
	}

	if !ValidName(newName) {
		return fmt.Errorf("invalid instance name %q: use Chinese, letters, digits, space or . _ -, at most 64 chars", newName)
	}
	oldPath := inst.cfgPath
	oldConfig, err := os.ReadFile(oldPath)
	if err != nil {
		return fmt.Errorf("read current config: %w", err)
	}

	tmpPath, err := writeTempConfig(filepath.Dir(oldPath), filepath.Base(oldPath), []byte(content))
	if err != nil {
		return err
	}
	cleanupTemp := func() {
		if tmpPath != "" {
			_ = os.Remove(tmpPath)
		}
	}
	defer cleanupTemp()

	check := newInstance(name, tmpPath, m.unsafe, m.logs)
	if _, _, _, _, err := check.buildAggregator(); err != nil {
		return fmt.Errorf("config invalid: %w", err)
	}

	wantRename := newName != name
	finalPath := oldPath
	if wantRename {
		finalPath = filepath.Join(m.dir, newName+".toml")
		m.mu.Lock()
		_, exists := m.instances[newName]
		m.mu.Unlock()
		if exists {
			return fmt.Errorf("instance %s already exists", newName)
		}
		if _, err := os.Stat(finalPath); err == nil {
			return fmt.Errorf("config file for %s already exists", newName)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("check config file for %s: %w", newName, err)
		}
	}

	inst.mu.Lock()
	wasRunning := inst.state == StateRunning
	inst.mu.Unlock()
	wasStopped := false
	if wantRename {
		m.mu.Lock()
		wasStopped = m.stopped[name]
		m.mu.Unlock()
	}

	// Keep a durable copy before installing the new one. The original file
	// remains in place until replacement succeeds, so a process crash cannot
	// leave the instance without any usable config.
	backupPath, err := writeTempConfig(filepath.Dir(oldPath), filepath.Base(oldPath)+".rollback", oldConfig)
	if err != nil {
		return fmt.Errorf("backup current config: %w", err)
	}
	backupExists := true
	defer func() {
		if backupExists {
			_ = os.Remove(backupPath)
		}
	}()

	if wasRunning {
		inst.stop()
	}
	if err := replaceFile(tmpPath, finalPath); err != nil {
		restoreErr := replaceFile(backupPath, oldPath)
		backupExists = false
		if restoreErr != nil {
			return fmt.Errorf("install new config: %v; restore previous config: %w", err, restoreErr)
		}
		if wasRunning {
			if restartErr := inst.start(); restartErr != nil {
				return fmt.Errorf("install new config: %v; previous config restored but restart failed: %w", err, restartErr)
			}
		}
		return fmt.Errorf("install new config: %w", err)
	}
	tmpPath = ""

	if wantRename {
		inst.mu.Lock()
		inst.Name = newName
		inst.cfgPath = finalPath
		inst.mu.Unlock()
		if m.logs != nil {
			m.logs.Rename(name, newName)
		}
		m.mu.Lock()
		delete(m.instances, name)
		m.instances[newName] = inst
		if wasStopped {
			delete(m.stopped, name)
			m.stopped[newName] = true
		}
		if err := m.saveState(); err != nil {
			log.Warnf("save state after renaming [%s] to [%s]: %v", name, newName, err)
		}
		m.mu.Unlock()
		log.Infof("instance [%s] renamed to [%s]", name, newName)
	}

	if wasRunning {
		if err := inst.start(); err != nil {
			rollbackErr := m.rollbackConfigUpdate(inst, name, newName, oldPath, finalPath, backupPath, wantRename, wasStopped)
			backupExists = false
			if rollbackErr != nil {
				return fmt.Errorf("new config failed to start: %v; rollback failed: %w", err, rollbackErr)
			}
			return fmt.Errorf("new config failed to start; previous config restored: %w", err)
		}
	}

	if wantRename {
		if err := os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
			log.Warnf("remove old config file of [%s]: %v", name, err)
		}
	}
	if err := os.Remove(backupPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove config backup: %w", err)
	}
	backupExists = false
	return nil
}

// rollbackConfigUpdate restores the old config and instance registration after
// the replacement config failed to start. The caller owns opMu, so no other
// mutating operation can observe the intermediate state.
func (m *Manager) rollbackConfigUpdate(inst *Instance, oldName, newName, oldPath, newPath, backupPath string, renamed, wasStopped bool) error {
	inst.stop()
	if err := os.Remove(newPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove failed config: %w", err)
	}
	if err := replaceFile(backupPath, oldPath); err != nil {
		return fmt.Errorf("restore old config: %w", err)
	}

	if renamed {
		inst.mu.Lock()
		inst.Name = oldName
		inst.cfgPath = oldPath
		inst.mu.Unlock()
		if m.logs != nil {
			m.logs.Rename(newName, oldName)
		}
		m.mu.Lock()
		delete(m.instances, newName)
		m.instances[oldName] = inst
		if wasStopped {
			delete(m.stopped, newName)
			m.stopped[oldName] = true
		}
		m.mu.Unlock()
		log.Infof("instance [%s] rollback to [%s]", newName, oldName)
	}

	if _, err := os.Stat(oldPath); err != nil {
		return fmt.Errorf("verify restored config: %w", err)
	}
	if wasStopped {
		return nil
	}
	if err := inst.start(); err != nil {
		return fmt.Errorf("restart previous config: %w", err)
	}
	return nil
}

func (m *Manager) GetConfig(name string) (string, error) {
	m.mu.Lock()
	inst, err := m.get(name)
	m.mu.Unlock()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(inst.cfgPath)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (m *Manager) Delete(name string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	inst, err := m.get(name)
	if err == nil {
		delete(m.instances, name)
	}
	delete(m.stopped, name)
	_ = m.saveState()
	m.mu.Unlock()
	if err != nil {
		return err
	}

	inst.stop()
	if m.logs != nil {
		m.logs.Remove(name)
	}
	if err := os.Remove(inst.cfgPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	log.Infof("instance [%s] deleted", name)
	return nil
}
