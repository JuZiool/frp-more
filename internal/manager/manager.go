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

func newInstance(name, cfgPath string, unsafe *security.UnsafeFeatures) *Instance {
	return &Instance{
		Name:    name,
		cfgPath: cfgPath,
		unsafe:  unsafe,
		state:   StateStopped,
	}
}

// loginFailExitRe matches user-set loginFailExit lines (top-level or misplaced),
// ignoring commented-out ones.
var loginFailExitRe = regexp.MustCompile(`(?m)^[ \t]*loginFailExit[ \t]*=.*(?:\r?\n)?`)

// loginFailExitBlock is the comment + key + blank line prepended to configs.
const loginFailExitBlock = "# 连接失败后保持重试，便于服务恢复后自动重连（由 FRP-More 强制保留）\n" +
	"loginFailExit = false\n\n"

// enforceLoginFailExit strips any user-set loginFailExit keys and prepends the
// enforced block, so connections always keep retrying after drops.
func enforceLoginFailExit(content string) string {
	return loginFailExitBlock + loginFailExitRe.ReplaceAllString(content, "")
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
		log.Warnf("instance [%s] config warning: %v", i.Name, warning)
	}
	if err != nil {
		return nil, nil, nil, nil, err
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
		return err
	}

	svr, err := client.NewService(client.ServiceOptions{
		Common:                 common,
		ConfigSourceAggregator: aggregator,
		UnsafeFeatures:         i.unsafe,
		ConfigFilePath:         i.cfgPath,
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
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
		defer i.mu.Unlock()
		if i.state == StateRunning { // exited on its own, not a manual stop
			i.state = StateExited
			if err != nil {
				i.lastErr = err.Error()
			}
		}
		i.svr = nil
		i.cancel = nil
	}()

	log.Infof("instance [%s] started (server %s:%d, %d proxies)",
		i.Name, common.ServerAddr, common.ServerPort, len(proxyCfgs))
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
			log.Warnf("instance [%s] did not stop within 3s", i.Name)
		}
	}
	log.Infof("instance [%s] stopped", i.Name)
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
	mu                 sync.Mutex
	dir                string
	statePath          string
	unsafe             *security.UnsafeFeatures
	instances          map[string]*Instance
	stopped            map[string]bool
	forceLoginFailExit bool
}

func NewManager(dataDir string) (*Manager, error) {
	dir := filepath.Join(dataDir, "instances")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	m := &Manager{
		dir:                 dir,
		statePath:           filepath.Join(dataDir, "state.json"),
		unsafe:              security.NewUnsafeFeatures(nil),
		instances:           map[string]*Instance{},
		stopped:             map[string]bool{},
		forceLoginFailExit:  true, // 默认开启：强制保留 loginFailExit = false
	}
	if err := m.loadState(); err != nil {
		log.Warnf("load state: %v", err)
	}
	return m, nil
}

type persistedState struct {
	Stopped  []string `json:"stopped"`
	Settings *Settings `json:"settings,omitempty"`
}

// Settings are manager-wide user preferences.
type Settings struct {
	ForceLoginFailExit bool `json:"forceLoginFailExit"`
}

func (m *Manager) Settings() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Settings{ForceLoginFailExit: m.forceLoginFailExit}
}

func (m *Manager) SetSettings(s Settings) error {
	m.mu.Lock()
	m.forceLoginFailExit = s.ForceLoginFailExit
	err := m.saveState()
	m.mu.Unlock()
	return err
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
	if st.Settings != nil {
		m.forceLoginFailExit = st.Settings.ForceLoginFailExit
	}
	return nil
}

func (m *Manager) saveState() error {
	st := persistedState{
		Stopped:  []string{},
		Settings: &Settings{ForceLoginFailExit: m.forceLoginFailExit},
	}
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
		inst := newInstance(name, filepath.Join(m.dir, e.Name()), m.unsafe)
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
	if !ValidName(name) {
		return fmt.Errorf("invalid instance name %q: use Chinese, letters, digits, space or . _ -, at most 64 chars", name)
	}
	m.mu.Lock()
	_, exists := m.instances[name]
	dir := m.dir
	force := m.forceLoginFailExit
	m.mu.Unlock()
	if exists {
		return fmt.Errorf("instance %s already exists", name)
	}
	if force {
		content = enforceLoginFailExit(content)
	}

	tmpPath := filepath.Join(dir, name+".toml.tmp")
	if err := os.WriteFile(tmpPath, []byte(content), 0o644); err != nil {
		return err
	}
	check := newInstance(name, tmpPath, m.unsafe)
	if _, _, _, _, err := check.buildAggregator(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("config invalid: %w", err)
	}

	finalPath := filepath.Join(dir, name+".toml")
	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	inst := newInstance(name, finalPath, m.unsafe)

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

// UpdateConfig replaces the instance config file (validated first); a running
// instance is restarted to apply it. When newName differs from the current
// name, the config file is renamed and the instance re-registered as well.
func (m *Manager) UpdateConfig(name, newName, content string) error {
	m.mu.Lock()
	inst, err := m.get(name)
	force := m.forceLoginFailExit
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if newName == "" {
		newName = name
	}
	if force {
		content = enforceLoginFailExit(content)
	}

	tmpPath := inst.cfgPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(content), 0o644); err != nil {
		return err
	}
	check := newInstance(name, tmpPath, m.unsafe)
	if _, _, _, _, err := check.buildAggregator(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("config invalid: %w", err)
	}

	wantRename := newName != name
	if wantRename {
		if !ValidName(newName) {
			os.Remove(tmpPath)
			return fmt.Errorf("invalid instance name %q: use Chinese, letters, digits, space or . _ -, at most 64 chars", newName)
		}
		m.mu.Lock()
		_, exists := m.instances[newName]
		m.mu.Unlock()
		if exists {
			os.Remove(tmpPath)
			return fmt.Errorf("instance %s already exists", newName)
		}
		if _, err := os.Stat(filepath.Join(m.dir, newName+".toml")); err == nil {
			os.Remove(tmpPath)
			return fmt.Errorf("config file for %s already exists", newName)
		}
	}

	inst.mu.Lock()
	wasRunning := inst.state == StateRunning
	inst.mu.Unlock()
	if wasRunning {
		inst.stop()
	}

	finalPath := inst.cfgPath
	if wantRename {
		finalPath = filepath.Join(m.dir, newName+".toml")
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		return err
	}

	if wantRename {
		if err := os.Remove(inst.cfgPath); err != nil {
			log.Warnf("remove old config file of [%s]: %v", name, err)
		}
		inst.mu.Lock()
		inst.Name = newName
		inst.cfgPath = finalPath
		inst.mu.Unlock()
		m.mu.Lock()
		delete(m.instances, name)
		m.instances[newName] = inst
		if m.stopped[name] {
			delete(m.stopped, name)
			m.stopped[newName] = true
		}
		_ = m.saveState()
		m.mu.Unlock()
		log.Infof("instance [%s] renamed to [%s]", name, newName)
	}

	if wasRunning {
		return inst.start()
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
	if err := os.Remove(inst.cfgPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	log.Infof("instance [%s] deleted", name)
	return nil
}
