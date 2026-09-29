// voidctl — systemctl + journalctl + timers do systemd sobre o runit.
//
// Um único arquivo, sem dependências além da stdlib:
//
//	go build -trimpath -ldflags "-s -w" -o voidctl voidctl.go
//	install -Dm755 voidctl /usr/bin/voidctl && voidctl setup
//	ln -s voidctl /usr/bin/systemctl; ln -s voidctl /usr/bin/journalctl   # opcional
//
// O runit continua supervisionando (runsvdir/runsv). O voidctl fala direto com
// o runsv pelos FIFOs supervise/control e lê o supervise/status binário.
//
// Uso rápido:
//
//	voidctl import --dry-run nginx        prévia do que seria gerado em /etc/sv/nginx
//	voidctl import --enable --now nginx   converte a unit, habilita e sobe
//	voidctl status nginx                  estado, PID, memória, processos, logs
//	voidctl logs -u nginx -f              como journalctl -fu nginx
//	voidctl import --enable backup.timer  timer + serviço + daemon voidctl-timerd
//	voidctl list-timers
//
// Mapeamento systemd -> runit:
//
//	ExecStart=                 exec chpst ... no run
//	ExecStartPre=/Post=        antes do exec / em segundo plano após subir
//	ExecStop=, KillSignal=     control/t (o runsv chama no lugar do SIGTERM)
//	ExecReload=                control/h (voidctl reload)
//	ExecStopPost=              finish
//	Restart=no                 o run grava 'o' em supervise/control (sv once)
//	Restart=on-failure/...     o finish avalia $1/$2 e grava 'd' se não deve reiniciar
//	RestartSec=                sleep no finish
//	Type=oneshot               start espera terminar e reporta o código de saída
//	Type=forking + PIDFile=    o run vigia o PID; control/t mata o daemon
//	User= Group= Nice= Limit*  chpst -u/-n/-o/-p e ulimit
//	Environment*= WorkingDirectory= UMask= *Directory=   export / cd / umask / install -d
//	Requires= BindsTo=         sv check no run; start sobe antes; stop derruba dependentes
//	Wants= After=              sv check só se a dependência estiver habilitada
//	journald                   svlogd -tt em /var/log/sv/NOME (+ socklog para serviços nativos)
//	.timer                     voidctl-timerd dispara 'sv up' no serviço
//
// Sem equivalente no runit (o import avisa): ativação por socket, sd_notify,
// watchdog, cgroups (MemoryMax=, CPUQuota=), sandboxing (ProtectSystem=...).
//
// Caminhos, sobrescrevíveis por ambiente: VOIDCTL_SVDIR (/etc/sv),
// VOIDCTL_RUNDIR (/var/service), VOIDCTL_LOGDIR (/var/log/sv),
// VOIDCTL_CONFDIR (/etc/voidctl), VOIDCTL_STATEDIR (/var/lib/voidctl),
// VOIDCTL_RUNTIMEDIR (/run/voidctl), VOIDCTL_SOCKLOGDIR (/var/log/socklog),
// VOIDCTL_UNITPATH, SVWAIT (timeout de start/stop, 7 s).
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ======================================================================
// CLI e despacho de comandos
// ======================================================================
const version = "0.1.0"

var quiet bool

func usage() {
	fmt.Printf(`%svoidctl%s %s — systemctl/journalctl para o runit

Uso: voidctl [opções] COMANDO [ARGS...]

%sServiços%s
  status [NOME...]              estado detalhado (sem nome: visão geral)
  start | stop | restart NOME…  sobe, para ou reinicia (respeitando dependências)
  reload NOME…                  recarrega (ExecReload ou SIGHUP)
  try-restart NOME…             reinicia só se estiver rodando
  kill [-s SINAL] NOME…         envia sinal ao processo principal
  enable  [--now] NOME…         habilita no boot (link em %s)
  disable [--now] NOME…         tira do boot (--now também para)
  mask | unmask NOME…           impede/permite qualquer start
  is-active | is-enabled | is-failed NOME…
  list-units [--all] [--failed] serviços (padrão: só os supervisionados)
  list-unit-files               todos os serviços e timers
  list-dependencies [--reverse] NOME
  cat NOME                      unit de origem + scripts runit gerados
  show [-p PROP] [--value] NOME propriedades chave=valor (para scripts)
  edit NOME                     edita a unit (ou o run nativo) e regenera

%sUnits do systemd%s
  import [--dry-run] [--force] [--enable] [--now] [--restart=POL] [--name=N] UNIT…
                                converte .service/.timer em serviço runit
  daemon-reload                 regenera serviços importados a partir das units

%sLogs%s
  logs | journal [-u NOME]… [-f] [-n N] [-r] [-b] [--since T] [--until T]
                 [-g REGEX] [-o short|short-iso|short-precise|cat|json] [--no-pager]

%sTimers%s
  list-timers [--all]           próximos disparos
  calendar EXPR…                valida um OnCalendar= e mostra os próximos disparos
  timespan SPAN…                valida um intervalo (ex: "1h 30min")
  timerd                        daemon de timers (roda como serviço %s)

%sSistema%s
  poweroff | reboot | halt
  setup                         instala o hook de boot e cria os diretórios

Opções globais: --no-block, --no-color, -q/--quiet, --version, -h/--help
Chamado como "journalctl" (symlink) funciona como 'voidctl logs'.
`, bold, reset, version, bold, reset, cfg.runDir, bold, reset, bold, reset, bold, reset, timerdName, bold, reset)
}

func main() {
	loadConfig()
	colorMode := "auto"
	var rest []string
	for _, a := range os.Args[1:] {
		switch a {
		case "--no-color":
			colorMode = "never"
		case "--color", "--color=always":
			colorMode = "always"
		case "-q", "--quiet":
			quiet = true
		case "--no-block":
			noBlock = true
		case "--system":
		case "--version":
			fmt.Println("voidctl", version)
			return
		default:
			rest = append(rest, a)
		}
	}
	initColors(colorMode)
	switch filepath.Base(os.Args[0]) {
	case "journalctl", "voidjournal":
		os.Exit(cmdLogs(rest))
	}
	if len(rest) == 0 {
		os.Exit(cmdListUnits(nil))
	}
	os.Exit(dispatch(rest[0], rest[1:]))
}

func dispatch(cmd string, args []string) int {
	parseNow := func() (bool, []string) {
		o, err := parseArgs(args, map[string]fdef{"--now": {"now", false}})
		if err != nil {
			die("%v", err)
		}
		return o.bool("now"), o.pos
	}
	switch cmd {
	case "-h", "--help", "help":
		usage()
		return 0
	case "status":
		return cmdStatus(args)
	case "start":
		return each(args, func(n, k string) error {
			if k == "timer" {
				return enableTimer(n)
			}
			return startUnit(n, map[string]bool{})
		})
	case "stop":
		return each(args, func(n, k string) error {
			if k == "timer" {
				return disableTimer(n)
			}
			return stopUnit(n, map[string]bool{})
		})
	case "restart":
		return each(args, func(n, k string) error { return restartUnit(n, false) })
	case "try-restart", "condrestart":
		return each(args, func(n, k string) error { return restartUnit(n, true) })
	case "reload":
		return each(args, func(n, k string) error {
			if err := mustExist(n); err != nil {
				return err
			}
			return svControl(n, "h")
		})
	case "reload-or-restart":
		return each(args, func(n, k string) error {
			if exists(filepath.Join(svDefDir(n), "control", "h")) {
				return svControl(n, "h")
			}
			return restartUnit(n, false)
		})
	case "kill":
		o, err := parseArgs(args, map[string]fdef{"-s": {"sig", true}, "--signal": {"sig", true}})
		if err != nil {
			errorf("%v", err)
			return 2
		}
		sig := o.str("sig")
		if sig == "" {
			sig = "TERM"
		}
		return each(o.pos, func(n, k string) error { return killUnit(n, sig) })
	case "enable":
		now, pos := parseNow()
		return each(pos, func(n, k string) error {
			if k == "timer" {
				return enableTimer(n)
			}
			return enableUnit(n, now)
		})
	case "disable":
		now, pos := parseNow()
		return each(pos, func(n, k string) error {
			if k == "timer" {
				return disableTimer(n)
			}
			return disableUnit(n, now)
		})
	case "mask":
		return each(args, func(n, k string) error { return maskUnit(n) })
	case "unmask":
		return each(args, func(n, k string) error { return unmaskUnit(n) })
	case "is-active", "is-enabled", "is-failed":
		return cmdIs(cmd, args)
	case "list-units", "list", "ls":
		return cmdListUnits(args)
	case "list-unit-files":
		return cmdListUnitFiles(args)
	case "list-dependencies":
		return cmdListDeps(args)
	case "cat":
		return cmdCat(args)
	case "show":
		return cmdShow(args)
	case "edit":
		return cmdEdit(args)
	case "import":
		return cmdImport(args)
	case "daemon-reload":
		return cmdDaemonReload(args)
	case "logs", "journal", "journalctl":
		return cmdLogs(args)
	case "list-timers":
		return cmdListTimers(args)
	case "calendar":
		return cmdCalendar(args)
	case "timespan":
		return cmdTimespan(args)
	case "timerd":
		return cmdTimerd(args)
	case "boot-cleanup":
		return cmdBootCleanup()
	case "setup":
		return cmdSetup()
	case "poweroff", "reboot", "halt":
		c := exec.Command(cmd)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			errorf("%v", err)
			return 1
		}
		return 0
	}
	errorf("comando desconhecido: %s (veja 'voidctl --help')", cmd)
	return 2
}

// ======================================================================
// configuração e caminhos
// ======================================================================
// config reúne todos os caminhos usados pelo voidctl. Tudo pode ser
// sobrescrito por variáveis de ambiente (útil para testes e chroots).
type config struct {
	svDir      string // definições dos serviços runit        (/etc/sv)
	runDir     string // serviços habilitados (runsvdir)       (/var/service)
	logDir     string // logs svlogd dos serviços gerados      (/var/log/sv)
	confDir    string // configuração do voidctl               (/etc/voidctl)
	unitDir    string // units .service/.timer do administrador (/etc/voidctl/system)
	timerDir   string // timers importados                     (/etc/voidctl/timers)
	maskDir    string // serviços mascarados                   (/etc/voidctl/masked)
	stateDir   string // estado persistente                    (/var/lib/voidctl)
	runtimeDir string // estado volátil                        (/run/voidctl)
	socklogDir string // logs do socklog (syslog)              (/var/log/socklog)
	unitPaths  []string
	wait       time.Duration
}

var cfg config

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func loadConfig() {
	cfg.svDir = getenv("VOIDCTL_SVDIR", "/etc/sv")
	cfg.runDir = getenv("VOIDCTL_RUNDIR", "/var/service")
	cfg.logDir = getenv("VOIDCTL_LOGDIR", "/var/log/sv")
	cfg.confDir = getenv("VOIDCTL_CONFDIR", "/etc/voidctl")
	cfg.unitDir = getenv("VOIDCTL_UNITDIR", filepath.Join(cfg.confDir, "system"))
	cfg.timerDir = getenv("VOIDCTL_TIMERDIR", filepath.Join(cfg.confDir, "timers"))
	cfg.maskDir = filepath.Join(cfg.confDir, "masked")
	cfg.stateDir = getenv("VOIDCTL_STATEDIR", "/var/lib/voidctl")
	cfg.runtimeDir = getenv("VOIDCTL_RUNTIMEDIR", "/run/voidctl")
	cfg.socklogDir = getenv("VOIDCTL_SOCKLOGDIR", "/var/log/socklog")
	cfg.unitPaths = []string{
		cfg.unitDir,
		"/etc/systemd/system",
		"/usr/local/lib/systemd/system",
		"/usr/lib/systemd/system",
		"/lib/systemd/system",
	}
	if p := os.Getenv("VOIDCTL_UNITPATH"); p != "" {
		cfg.unitPaths = append([]string{cfg.unitDir}, filepath.SplitList(p)...)
	}
	cfg.wait = 7 * time.Second
	if v, err := strconv.Atoi(os.Getenv("SVWAIT")); err == nil && v > 0 {
		cfg.wait = time.Duration(v) * time.Second
	}
}

// ======================================================================
// parser de argumentos
// ======================================================================
type fdef struct {
	key string
	val bool // recebe valor?
}

type opts struct {
	b   map[string]bool
	s   map[string][]string
	pos []string
}

func (o *opts) bool(k string) bool { return o.b[k] }

func (o *opts) str(k string) string {
	if v := o.s[k]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

func (o *opts) strs(k string) []string { return o.s[k] }

func (o *opts) int(k string, def int) (int, error) {
	v := o.str(k)
	if v == "" {
		return def, nil
	}
	return strconv.Atoi(strings.TrimPrefix(v, "+"))
}

// parseArgs aceita flags em qualquer posição, --flag=valor, --flag valor,
// -n10, -n 10 e flags curtas agrupadas (-fr).
func parseArgs(args []string, defs map[string]fdef) (*opts, error) {
	o := &opts{b: map[string]bool{}, s: map[string][]string{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			o.pos = append(o.pos, args[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' {
			o.pos = append(o.pos, a)
			continue
		}
		if strings.HasPrefix(a, "--") {
			k, v, hasV := strings.Cut(a, "=")
			d, ok := defs[k]
			if !ok {
				return nil, fmt.Errorf("opção desconhecida: %s", k)
			}
			if d.val {
				if !hasV {
					if i+1 >= len(args) {
						return nil, fmt.Errorf("%s exige um valor", k)
					}
					i++
					v = args[i]
				}
				o.s[d.key] = append(o.s[d.key], v)
			} else {
				o.b[d.key] = true
			}
			continue
		}
		// curtas
		for j := 1; j < len(a); j++ {
			k := "-" + string(a[j])
			d, ok := defs[k]
			if !ok {
				// número negativo como posicional (ex: --since -1h tratado acima)
				return nil, fmt.Errorf("opção desconhecida: %s", k)
			}
			if d.val {
				v := a[j+1:]
				if v == "" {
					if i+1 >= len(args) {
						return nil, fmt.Errorf("%s exige um valor", k)
					}
					i++
					v = args[i]
				}
				o.s[d.key] = append(o.s[d.key], v)
				break
			}
			o.b[d.key] = true
		}
	}
	return o, nil
}

// ======================================================================
// cores, mensagens e formatação
// ======================================================================
// cores (nomes em minúsculo, por convenção do projeto)
var (
	red, green, yellow, blue, cyan, gray, bold, reset string
)

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func initColors(mode string) {
	if mode == "never" || os.Getenv("NO_COLOR") != "" {
		return
	}
	if mode != "always" && !isTTY(os.Stdout) {
		return
	}
	red = "\033[31m"
	green = "\033[32m"
	yellow = "\033[33m"
	blue = "\033[34m"
	cyan = "\033[36m"
	gray = "\033[90m"
	bold = "\033[1m"
	reset = "\033[0m"
}

func errorf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, red+"voidctl:"+reset+" "+format+"\n", a...)
}

func warnf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, yellow+"aviso:"+reset+" "+format+"\n", a...)
}

func notef(format string, a ...any) {
	if quiet {
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", a...)
}

func die(format string, a ...any) {
	errorf(format, a...)
	os.Exit(1)
}

// fmtSpan formata uma duração no estilo do systemd ("1 day 3h", "5min 12s").
func fmtSpan(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	type unit struct {
		n    time.Duration
		s, p string
	}
	units := []unit{
		{365 * 24 * time.Hour, " year", " years"},
		{30 * 24 * time.Hour, " month", " months"},
		{7 * 24 * time.Hour, " week", " weeks"},
		{24 * time.Hour, " day", " days"},
		{time.Hour, "h", "h"},
		{time.Minute, "min", "min"},
		{time.Second, "s", "s"},
	}
	var parts []string
	for _, u := range units {
		if d >= u.n {
			n := d / u.n
			d -= n * u.n
			suf := u.s
			if n > 1 {
				suf = u.p
			}
			parts = append(parts, fmt.Sprintf("%d%s", n, suf))
			if len(parts) == 2 {
				break
			}
		} else if len(parts) > 0 {
			break
		}
	}
	return strings.Join(parts, " ")
}

func fmtStamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("Mon 2006-01-02 15:04:05 MST")
}

func pad(s string, n int) string {
	if l := len([]rune(s)); l < n {
		return s + strings.Repeat(" ", n-l)
	}
	return s
}

// printTable imprime colunas alinhadas; cabeçalho em negrito.
func printTable(head []string, rows [][]string) {
	w := make([]int, len(head))
	for i, h := range head {
		w[i] = len([]rune(h))
	}
	for _, r := range rows {
		for i, c := range r {
			if l := len([]rune(stripANSI(c))); l > w[i] {
				w[i] = l
			}
		}
	}
	line := func(cols []string, isHead bool) {
		var b strings.Builder
		for i, c := range cols {
			if i == len(cols)-1 {
				b.WriteString(c)
				break
			}
			b.WriteString(c)
			b.WriteString(strings.Repeat(" ", w[i]-len([]rune(stripANSI(c)))+1))
		}
		if isHead {
			fmt.Println(bold + strings.TrimRight(b.String(), " ") + reset)
		} else {
			fmt.Println(strings.TrimRight(b.String(), " "))
		}
	}
	line(head, true)
	for _, r := range rows {
		line(r, false)
	}
}

func stripANSI(s string) string {
	if !strings.Contains(s, "\033[") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// shq devolve s entre aspas simples, seguro para sh.
func shq(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@%+,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ======================================================================
// /proc: processos, memória, boot
// ======================================================================
type procInfo struct {
	pid, ppid int
	comm      string
}

func listProcs() map[int]procInfo {
	m := map[int]procInfo{}
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(b)
		l, r := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if l < 0 || r < 0 || r+2 >= len(s) {
			continue
		}
		f := strings.Fields(s[r+2:])
		if len(f) < 2 {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		m[pid] = procInfo{pid, ppid, s[l+1 : r]}
	}
	return m
}

// procTree devolve root e todos os descendentes, com profundidade.
func procTree(root int, procs map[int]procInfo) (pids []int, depth map[int]int) {
	kids := map[int][]int{}
	for _, p := range procs {
		kids[p.ppid] = append(kids[p.ppid], p.pid)
	}
	depth = map[int]int{}
	var walk func(p, d int)
	walk = func(p, d int) {
		pids = append(pids, p)
		depth[p] = d
		k := kids[p]
		sort.Ints(k)
		for _, c := range k {
			walk(c, d+1)
		}
	}
	if _, ok := procs[root]; ok {
		walk(root, 0)
	}
	return
}

func procCmdline(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil || len(b) == 0 {
		if p, ok := listProcs()[pid]; ok {
			return "[" + p.comm + "]"
		}
		return ""
	}
	return strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " "))
}

func procComm(pid int) string {
	b, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	return strings.TrimSpace(string(b))
}

func procRSS(pid int) int64 {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/statm")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0
	}
	n, _ := strconv.ParseInt(f[1], 10, 64)
	return n * int64(os.Getpagesize())
}

func procTasks(pid int) int {
	e, _ := os.ReadDir("/proc/" + strconv.Itoa(pid) + "/task")
	return len(e)
}

func bootTime() time.Time {
	b, _ := os.ReadFile("/proc/stat")
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "btime ") {
			n, _ := strconv.ParseInt(strings.TrimSpace(l[6:]), 10, 64)
			return time.Unix(n, 0)
		}
	}
	return time.Time{}
}

func fmtBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64) + "G"
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + "M"
	case n >= 1<<10:
		return strconv.FormatFloat(float64(n)/(1<<10), 'f', 1, 64) + "K"
	}
	return strconv.FormatInt(n, 10) + "B"
}

// ======================================================================
// runsv: status binário, controle, estado
// ======================================================================
// Deslocamento TAI64 usado pelo runsv (2^62 + 10 segundos de leap).
const taiOffset = 4611686018427387914

// svStatus espelha o arquivo binário supervise/status (20 bytes) do runsv.
type svStatus struct {
	supervised bool
	state      int // 0 down, 1 run, 2 finish
	pid        int
	since      time.Time
	paused     bool
	want       byte // 'u' ou 'd'
	term       bool
}

const (
	stDown   = 0
	stRun    = 1
	stFinish = 2
)

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func lexists(p string) bool { _, err := os.Lstat(p); return err == nil }

func isDir(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() }

func svDefDir(name string) string { return filepath.Join(cfg.svDir, name) }
func svLink(name string) string   { return filepath.Join(cfg.runDir, name) }

// svPath devolve o diretório do serviço usado para status/controle.
func svPath(name string) string {
	if p := svLink(name); exists(p) {
		return p
	}
	return svDefDir(name)
}

func serviceExists(name string) bool {
	return exists(filepath.Join(svDefDir(name), "run")) || exists(filepath.Join(svLink(name), "run"))
}

func isLinked(name string) bool { return lexists(svLink(name)) }

func isMasked(name string) bool { return lexists(filepath.Join(cfg.maskDir, name)) }

func runtimeMarker(name string) string { return filepath.Join(cfg.stateDir, "runtime", name) }

func isRuntime(name string) bool { return exists(runtimeMarker(name)) }

// isSupervised verifica se há um runsv ativo lendo supervise/ok (FIFO).
func isSupervised(dir string) bool {
	f, err := os.OpenFile(filepath.Join(dir, "supervise", "ok"), os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err == nil {
		f.Close()
		return true
	}
	// sem permissão (usuário comum): assume supervisionado se houver status
	if errors.Is(err, os.ErrPermission) {
		return exists(filepath.Join(dir, "supervise", "status"))
	}
	return false
}

func readStatus(name string) svStatus {
	dir := svPath(name)
	st := svStatus{supervised: isSupervised(dir)}
	b, err := os.ReadFile(filepath.Join(dir, "supervise", "status"))
	if err != nil || len(b) < 20 {
		return st
	}
	tai := binary.BigEndian.Uint64(b[0:8])
	nano := binary.BigEndian.Uint32(b[8:12])
	if tai > taiOffset {
		st.since = time.Unix(int64(tai-taiOffset), int64(nano))
	}
	st.pid = int(binary.LittleEndian.Uint32(b[12:16]))
	st.paused = b[16] != 0
	st.want = b[17]
	st.term = b[18] != 0
	st.state = int(b[19])
	if !st.supervised {
		st.state, st.pid = stDown, 0
	}
	return st
}

// svControl escreve comandos no FIFO supervise/control (como o sv faz).
func svControl(name, cmd string) error {
	dir := svPath(name)
	f, err := os.OpenFile(filepath.Join(dir, "supervise", "control"), os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ENXIO) || os.IsNotExist(err) {
			return fmt.Errorf("%s não está sendo supervisionado (runsv não está rodando)", name)
		}
		return err
	}
	defer f.Close()
	_, err = f.WriteString(cmd)
	return err
}

// waitStatus espera até cond ser verdadeira ou o timeout expirar.
func waitStatus(name string, timeout time.Duration, cond func(svStatus) bool) (svStatus, bool) {
	deadline := time.Now().Add(timeout)
	for {
		st := readStatus(name)
		if cond(st) {
			return st, true
		}
		if time.Now().After(deadline) {
			return st, false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ---------- metadados gerados pelo voidctl ----------

func metaPath(name string) string { return filepath.Join(svDefDir(name), "voidctl.meta") }

func readMeta(name string) map[string]string {
	f, err := os.Open(metaPath(name))
	if err != nil {
		return nil
	}
	defer f.Close()
	m := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if ok && !strings.HasPrefix(k, "#") {
			m[k] = v
		}
	}
	return m
}

func writeMeta(path string, m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# metadados do voidctl — não edite\n")
	for _, k := range keys {
		b.WriteString(k + "=" + m[k] + "\n")
	}
	return b.String()
}

var svDepRe = regexp.MustCompile(`\bsv\s+(?:-[a-zA-Z]+\s+(?:\d+\s+)?)*(?:check|start|up|status|once|o)\s+([A-Za-z0-9_@.:+-]+)`)

// depsOf devolve (requires, wants) de um serviço. Para serviços importados
// usa os metadados; para serviços nativos procura "sv check X" no run.
func depsOf(name string) (req, wants []string) {
	if m := readMeta(name); m != nil {
		return strings.Fields(m["requires"]), strings.Fields(m["wants"])
	}
	b, err := os.ReadFile(filepath.Join(svDefDir(name), "run"))
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, mm := range svDepRe.FindAllStringSubmatch(string(b), -1) {
		d := strings.TrimSuffix(filepath.Base(mm[1]), ".service")
		if d != name && d != "." && d != "$PWD" && !seen[d] {
			seen[d] = true
			req = append(req, d)
		}
	}
	return
}

func listServiceNames(all bool) []string {
	set := map[string]bool{}
	if all {
		ents, _ := os.ReadDir(cfg.svDir)
		for _, e := range ents {
			if exists(filepath.Join(cfg.svDir, e.Name(), "run")) {
				set[e.Name()] = true
			}
		}
	}
	ents, _ := os.ReadDir(cfg.runDir)
	for _, e := range ents {
		set[e.Name()] = true
	}
	var out []string
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func reverseRequires(name string) []string {
	var out []string
	for _, s := range listServiceNames(true) {
		req, _ := depsOf(s)
		for _, r := range req {
			if r == name {
				out = append(out, s)
			}
		}
	}
	return out
}

// ---------- resultado da última execução (gravado pelo finish gerado) ----------

type exitInfo struct {
	ok        bool
	code, sig int
	at        time.Time
}

func readExit(name string) exitInfo {
	b, err := os.ReadFile(filepath.Join(cfg.runtimeDir, name+".exit"))
	if err != nil {
		return exitInfo{}
	}
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return exitInfo{}
	}
	c, _ := strconv.Atoi(f[0])
	s, _ := strconv.Atoi(f[1])
	t, _ := strconv.ParseInt(f[2], 10, 64)
	return exitInfo{true, c, s, time.Unix(t, 0)}
}

func (e exitInfo) clean() bool {
	if e.code == 0 {
		return true
	}
	if e.code == -1 {
		switch e.sig {
		case 1, 2, 13, 15: // HUP INT PIPE TERM contam como saída limpa no systemd
			return true
		}
	}
	return false
}

func (e exitInfo) String() string {
	if e.code == -1 {
		return fmt.Sprintf("code=killed, signal=%s", sigName(e.sig))
	}
	s := "SUCCESS"
	if e.code != 0 {
		s = "FAILURE"
	}
	return fmt.Sprintf("code=exited, status=%d/%s", e.code, s)
}

var sigNames = map[int]string{1: "HUP", 2: "INT", 3: "QUIT", 6: "ABRT", 9: "KILL", 10: "USR1", 11: "SEGV", 12: "USR2", 13: "PIPE", 14: "ALRM", 15: "TERM", 18: "CONT", 19: "STOP"}

func sigName(n int) string {
	if s, ok := sigNames[n]; ok {
		return s
	}
	return strconv.Itoa(n)
}

// ---------- estado consolidado, com a nomenclatura do systemd ----------

type unitState struct {
	name     string
	exists   bool
	st       svStatus
	meta     map[string]string
	enabled  string
	active   string
	sub      string
	exit     exitInfo
	normalUp bool
}

func getState(name string) unitState {
	u := unitState{name: name, exists: serviceExists(name)}
	if !u.exists {
		u.enabled, u.active, u.sub = "not-found", "inactive", "dead"
		return u
	}
	u.meta = readMeta(name)
	u.st = readStatus(name)
	u.exit = readExit(name)
	u.normalUp = !exists(filepath.Join(svDefDir(name), "down"))

	switch {
	case isMasked(name):
		u.enabled = "masked"
	case u.meta != nil && u.meta["timer"] != "" && isLinked(name):
		u.enabled = "static"
	case isLinked(name) && isRuntime(name):
		u.enabled = "disabled"
	case isLinked(name):
		u.enabled = "enabled"
	default:
		u.enabled = "disabled"
	}

	st := u.st
	oneshotRemain := u.meta != nil && u.meta["type"] == "oneshot" && u.meta["remain"] == "yes"
	switch {
	case !st.supervised:
		u.active, u.sub = "inactive", "dead"
		if u.exit.ok && !u.exit.clean() {
			u.active, u.sub = "failed", "failed"
		}
	case st.state == stRun && st.want == 'd' && st.term:
		u.active, u.sub = "deactivating", "stop-sigterm"
	case st.state == stRun && st.paused:
		u.active, u.sub = "active", "paused"
	case st.state == stRun && oneshotRemain:
		u.active, u.sub = "active", "exited"
	case st.state == stRun:
		u.active, u.sub = "active", "running"
		// laço de falhas: acabou de reiniciar depois de uma saída com erro
		if u.exit.ok && !u.exit.clean() && time.Since(st.since) < 2*time.Second &&
			u.exit.at.After(st.since.Add(-3*time.Second)) {
			u.active, u.sub = "activating", "auto-restart"
		}
	case st.state == stFinish && st.want == 'u':
		u.active, u.sub = "activating", "auto-restart"
	case st.state == stFinish:
		u.active, u.sub = "deactivating", "stop-post"
	case st.want == 'u':
		u.active, u.sub = "activating", "auto-restart"
	case u.exit.ok && !u.exit.clean():
		u.active, u.sub = "failed", "failed"
	default:
		u.active, u.sub = "inactive", "dead"
	}
	return u
}

func (u unitState) dot() string {
	switch u.active {
	case "active":
		return green + "●" + reset
	case "failed":
		return red + "×" + reset
	case "activating", "deactivating":
		return yellow + "●" + reset
	}
	return "○"
}

func (u unitState) activeColored() string {
	s := u.active
	switch u.active {
	case "active":
		s = bold + green + s + reset
	case "failed":
		s = bold + red + s + reset
	case "activating", "deactivating":
		s = bold + yellow + s + reset
	}
	return s
}

func (u unitState) description() string {
	if u.meta != nil && u.meta["description"] != "" {
		return u.meta["description"]
	}
	if u.meta == nil {
		return "serviço runit " + u.name
	}
	return u.name
}

// ======================================================================
// parser de units do systemd
// ======================================================================
// unitFile é uma unit do systemd já mesclada com seus drop-ins.
type unitFile struct {
	name     string // nome completo, ex: foo@bar.service
	path     string
	sources  []string
	sec      map[string]map[string][]string
	raw      strings.Builder
	prefix   string
	instance string
}

var unitExts = []string{".service", ".timer", ".socket", ".path", ".target", ".mount"}

func hasUnitExt(s string) bool {
	for _, e := range unitExts {
		if strings.HasSuffix(s, e) {
			return true
		}
	}
	return false
}

func newUnit(name string) *unitFile {
	u := &unitFile{name: name, sec: map[string]map[string][]string{}}
	base := strings.TrimSuffix(name, filepath.Ext(name))
	if i := strings.Index(base, "@"); i >= 0 {
		u.prefix, u.instance = base[:i], base[i+1:]
	} else {
		u.prefix = base
	}
	return u
}

func (u *unitFile) base() string { return strings.TrimSuffix(u.name, filepath.Ext(u.name)) }

// load lê um arquivo INI do systemd e mescla em u.
func (u *unitFile) load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	u.sources = append(u.sources, path)
	fmt.Fprintf(&u.raw, "# %s\n%s\n", path, strings.TrimRight(string(data), "\n"))
	cur := ""
	cont := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, " \t\r")
		t := strings.TrimSpace(line)
		if cont == "" && (t == "" || t[0] == '#' || t[0] == ';') {
			continue
		}
		if cont != "" && (t != "" && (t[0] == '#' || t[0] == ';')) {
			continue // comentário dentro de continuação
		}
		if strings.HasSuffix(line, "\\") {
			cont += strings.TrimSuffix(line, "\\") + " "
			continue
		}
		t = strings.TrimSpace(cont + line)
		cont = ""
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			cur = t[1 : len(t)-1]
			if u.sec[cur] == nil {
				u.sec[cur] = map[string][]string{}
			}
			continue
		}
		k, v, ok := strings.Cut(t, "=")
		if !ok || cur == "" {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if v == "" { // atribuição vazia zera a lista (semântica do systemd)
			delete(u.sec[cur], k)
			continue
		}
		u.sec[cur][k] = append(u.sec[cur][k], v)
	}
	return nil
}

func (u *unitFile) rawGet(sec, key string) string {
	v := u.sec[sec][key]
	if len(v) == 0 {
		return ""
	}
	return v[len(v)-1]
}

func (u *unitFile) get(sec, key string) string { return u.expand(u.rawGet(sec, key)) }

func (u *unitFile) all(sec, key string) []string {
	var out []string
	for _, v := range u.sec[sec][key] {
		out = append(out, u.expand(v))
	}
	return out
}

func (u *unitFile) words(sec, key string) []string {
	var out []string
	for _, v := range u.all(sec, key) {
		out = append(out, splitQuoted(v)...)
	}
	return out
}

func (u *unitFile) has(sec, key string) bool { return len(u.sec[sec][key]) > 0 }

// expand resolve os especificadores % mais comuns.
func (u *unitFile) expand(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteString(u.name)
		case 'N':
			b.WriteString(u.base())
		case 'p':
			b.WriteString(u.prefix)
		case 'i', 'I':
			b.WriteString(u.instance)
		case 'j', 'J':
			p := u.prefix
			if k := strings.LastIndex(p, "-"); k >= 0 {
				p = p[k+1:]
			}
			b.WriteString(p)
		case 't':
			b.WriteString("/run")
		case 'S':
			b.WriteString("/var/lib")
		case 'C':
			b.WriteString("/var/cache")
		case 'L':
			b.WriteString("/var/log")
		case 'E':
			b.WriteString("/etc")
		case 'T':
			b.WriteString("/tmp")
		case 'V':
			b.WriteString("/var/tmp")
		case 'u':
			if n := u.rawGet("Service", "User"); n != "" {
				b.WriteString(n)
			} else {
				b.WriteString("root")
			}
		case 'U':
			name := u.rawGet("Service", "User")
			if name == "" {
				b.WriteString("0")
			} else if us, err := user.Lookup(name); err == nil {
				b.WriteString(us.Uid)
			} else {
				b.WriteString(name)
			}
		case 'h':
			name := u.rawGet("Service", "User")
			if name == "" {
				b.WriteString("/root")
			} else if us, err := user.Lookup(name); err == nil {
				b.WriteString(us.HomeDir)
			} else {
				b.WriteString("$HOME")
			}
		case 'H', 'l':
			h, _ := os.Hostname()
			b.WriteString(h)
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// splitQuoted separa palavras respeitando aspas simples e duplas.
func splitQuoted(s string) []string {
	var out []string
	var cur strings.Builder
	var q byte
	in := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == q {
				q = 0
			} else if c == '\\' && q == '"' && i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
			} else {
				cur.WriteByte(c)
			}
		case c == '"' || c == '\'':
			q, in = c, true
		case c == ' ' || c == '\t':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			in = true
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

// findUnit localiza uma unit por caminho ou nome nos diretórios padrão.
// Devolve o caminho do arquivo e o nome da unit (instanciado, se template).
func findUnit(arg string) (string, string, error) {
	if strings.Contains(arg, "/") {
		if !exists(arg) {
			return "", "", fmt.Errorf("arquivo %s não existe", arg)
		}
		return arg, filepath.Base(arg), nil
	}
	name := arg
	if !hasUnitExt(name) {
		name += ".service"
	}
	for _, d := range cfg.unitPaths {
		if p := filepath.Join(d, name); exists(p) {
			return p, name, nil
		}
	}
	// template: foo@bar.service -> foo@.service
	base := strings.TrimSuffix(name, filepath.Ext(name))
	if i := strings.Index(base, "@"); i >= 0 && i < len(base)-1 {
		tmpl := base[:i+1] + filepath.Ext(name)
		for _, d := range cfg.unitPaths {
			if p := filepath.Join(d, tmpl); exists(p) {
				return p, name, nil
			}
		}
	}
	return "", "", fmt.Errorf("unit %s não encontrada em %s", name, strings.Join(cfg.unitPaths, ":"))
}

// loadUnit carrega a unit e todos os drop-ins (*.d/*.conf).
func loadUnit(path, name string) (*unitFile, error) {
	if target, err := filepath.EvalSymlinks(path); err == nil && target == "/dev/null" {
		return nil, fmt.Errorf("%s está mascarada no systemd (link para /dev/null)", name)
	}
	u := newUnit(name)
	u.path = path
	if err := u.load(path); err != nil {
		return nil, err
	}
	ext := filepath.Ext(name)
	dirs := []string{}
	if u.instance != "" {
		dirs = append(dirs, u.prefix+"@"+ext+".d")
	}
	dirs = append(dirs, name+".d")
	search := append([]string{filepath.Dir(path)}, cfg.unitPaths...)
	for _, dd := range dirs {
		chosen := map[string]string{}
		for i := len(search) - 1; i >= 0; i-- { // os primeiros da lista têm precedência
			matches, _ := filepath.Glob(filepath.Join(search[i], dd, "*.conf"))
			for _, m := range matches {
				chosen[filepath.Base(m)] = m
			}
		}
		keys := make([]string, 0, len(chosen))
		for k := range chosen {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := u.load(chosen[k]); err != nil {
				return nil, err
			}
		}
	}
	return u, nil
}

func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "yes", "y", "true", "t", "on":
		return true
	}
	return false
}

// ======================================================================
// conversão .service -> runit, import, daemon-reload
// ======================================================================
type importOpts struct {
	name    string // nome do serviço runit (padrão: nome da unit)
	restart string // força política de Restart=
	timer   string // timer que ativa este serviço
	force   bool
	dryRun  bool
}

type genFile struct {
	rel     string
	content string
	mode    os.FileMode
}

type genResult struct {
	name  string
	files []genFile
	warns []string
	meta  map[string]string
	down  bool
	raw   string
}

// Alvos e units do systemd que não têm equivalente no runit (o estágio 1
// do runit já cobre) — dependências para eles são descartadas.
var ignoredDeps = map[string]bool{
	"network.target": true, "network-online.target": true, "network-pre.target": true,
	"nss-lookup.target": true, "nss-user-lookup.target": true, "remote-fs.target": true,
	"local-fs.target": true, "sysinit.target": true, "basic.target": true,
	"multi-user.target": true, "graphical.target": true, "time-sync.target": true,
	"syslog.target": true, "sockets.target": true, "timers.target": true,
	"paths.target": true, "shutdown.target": true, "default.target": true,
	"systemd-journald.socket": true, "syslog.socket": true, "remote-fs-pre.target": true,
	"local-fs-pre.target": true, "umount.target": true, "rpcbind.target": true,
	"systemd-tmpfiles-setup.service": true, "systemd-udev-settle.service": true,
	"systemd-networkd-wait-online.service": true, "NetworkManager-wait-online.service": true,
	"systemd-user-sessions.service": true, "systemd-remount-fs.service": true,
}

var depAlias = map[string]string{
	"dbus.socket":   "dbus",
	"dbus.service":  "dbus",
	"docker.socket": "docker",
}

func mapDeps(vals []string, warns *[]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range vals {
		if ignoredDeps[v] {
			continue
		}
		d := ""
		switch {
		case depAlias[v] != "":
			d = depAlias[v]
		case strings.HasSuffix(v, ".service"):
			d = strings.TrimSuffix(v, ".service")
		case strings.HasPrefix(v, "systemd-"):
			continue
		default:
			*warns = append(*warns, fmt.Sprintf("dependência %s ignorada (tipo sem equivalente no runit)", v))
			continue
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

// execLine interpreta os prefixos especiais das linhas Exec*=.
type execLine struct {
	cmd        string
	ignoreFail bool
	privileged bool
}

func parseExec(s string) execLine {
	var e execLine
	argv0 := false
loop:
	for len(s) > 0 {
		switch s[0] {
		case '-':
			e.ignoreFail = true
		case '+', '!':
			e.privileged = true
		case '@':
			argv0 = true
		case ':', '|':
		default:
			break loop
		}
		s = s[1:]
	}
	s = strings.TrimSpace(s)
	if argv0 { // "@/bin/prog argv0 args" -> descarta argv0
		if i := strings.IndexAny(s, " \t"); i > 0 {
			rest := strings.TrimLeft(s[i:], " \t")
			if j := strings.IndexAny(rest, " \t"); j > 0 {
				s = s[:i] + " " + strings.TrimLeft(rest[j:], " \t")
			} else {
				s = s[:i]
			}
		}
	}
	s = mainpidRe.ReplaceAllString(s, "$$MAINPID")
	e.cmd = s
	return e
}

var mainpidRe = regexp.MustCompile(`\$\{MAINPID\}`)

func (e execLine) sh(pfx string, abort string) string {
	c := e.cmd
	if !e.privileged && pfx != "" {
		c = pfx + c
	}
	if e.ignoreFail {
		return c + " || true\n"
	}
	return c + " || " + abort + "\n"
}

func header(src string) string {
	return fmt.Sprintf("#!/bin/sh\n# Gerado por voidctl a partir de %s\n# Não edite: altere a unit e rode 'voidctl daemon-reload' (ou 'voidctl edit').\n", src)
}

const wantUpFn = `want_up() { [ "$(od -An -c -j17 -N1 supervise/status 2>/dev/null | tr -d ' ')" = u ]; }
`

func convertService(u *unitFile, o importOpts) (*genResult, error) {
	name := o.name
	if name == "" {
		name = u.base()
	}
	r := &genResult{name: name, meta: map[string]string{}}
	g := func(k string) string { return u.get("Service", k) }
	src := u.path

	starts := u.all("Service", "ExecStart")
	typ := strings.ToLower(g("Type"))
	if typ == "" {
		switch {
		case len(starts) == 0:
			typ = "oneshot"
		case g("BusName") != "":
			typ = "dbus"
		default:
			typ = "simple"
		}
	}
	if len(starts) == 0 && typ != "oneshot" {
		return nil, fmt.Errorf("%s: sem ExecStart=", u.name)
	}
	if len(starts) > 1 && typ != "oneshot" {
		return nil, fmt.Errorf("%s: múltiplos ExecStart= só são permitidos com Type=oneshot", u.name)
	}
	restart := strings.ToLower(g("Restart"))
	if restart == "" {
		restart = "no"
	}
	if o.restart != "" {
		restart = o.restart
	}
	switch restart {
	case "no", "always", "on-success", "on-failure", "on-abnormal", "on-abort", "on-watchdog":
	default:
		return nil, fmt.Errorf("Restart=%s desconhecido", restart)
	}
	if restart == "on-watchdog" {
		r.warns = append(r.warns, "Restart=on-watchdog tratado como Restart=no (sem watchdog no runit)")
		restart = "no"
	}
	remain := parseBool(g("RemainAfterExit"))

	switch typ {
	case "notify", "notify-reload":
		r.warns = append(r.warns, "Type=notify: não há sd_notify no runit; tratado como simple")
	case "dbus":
		r.warns = append(r.warns, "Type=dbus tratado como simple")
	case "forking":
		if g("PIDFile") == "" {
			return nil, fmt.Errorf("%s: Type=forking sem PIDFile= não pode ser supervisionado; "+
				"adicione PIDFile= num drop-in ou rode o daemon em primeiro plano (Type=simple)", u.name)
		}
	case "simple", "exec", "idle", "oneshot":
	default:
		return nil, fmt.Errorf("Type=%s não suportado", typ)
	}
	for _, k := range []string{"WatchdogSec", "StartLimitBurst", "ProtectSystem", "ProtectHome", "PrivateTmp", "NoNewPrivileges", "CapabilityBoundingSet", "AmbientCapabilities", "MemoryMax", "CPUQuota"} {
		if u.has("Service", k) || u.has("Unit", k) {
			r.warns = append(r.warns, k+"= ignorado (sem equivalente no runit)")
		}
	}

	// ---- dependências ----
	var dw []string
	req := mapDeps(append(append(u.words("Unit", "Requires"), u.words("Unit", "BindsTo")...), u.words("Unit", "Requisite")...), &dw)
	wants := mapDeps(u.words("Unit", "Wants"), &dw)
	after := mapDeps(u.words("Unit", "After"), &dw)
	r.warns = append(r.warns, dw...)
	isReq := map[string]bool{}
	for _, d := range req {
		isReq[d] = true
	}
	var soft []string
	seen := map[string]bool{}
	for _, d := range append(append([]string{}, wants...), after...) {
		if !isReq[d] && !seen[d] && d != name {
			seen[d] = true
			soft = append(soft, d)
		}
	}

	// ---- chpst ----
	usr, grp := g("User"), g("Group")
	if parseBool(g("DynamicUser")) && usr == "" {
		usr = "nobody"
		r.warns = append(r.warns, "DynamicUser=yes: usando o usuário nobody")
	}
	var chp []string
	if usr != "" || grp != "" {
		spec := usr
		if spec == "" {
			spec = "root"
		}
		if grp != "" {
			spec += ":" + grp
		}
		for _, sg := range u.words("Service", "SupplementaryGroups") {
			spec += ":" + sg
		}
		chp = append(chp, "-u", spec)
	}
	if v := g("Nice"); v != "" {
		chp = append(chp, "-n", v)
	}
	if v := g("LimitNOFILE"); v != "" && v != "infinity" {
		chp = append(chp, "-o", strings.Split(v, ":")[0])
	}
	if v := g("LimitNPROC"); v != "" && v != "infinity" {
		chp = append(chp, "-p", strings.Split(v, ":")[0])
	}
	pfx := ""
	if len(chp) > 0 {
		q := make([]string, len(chp))
		for i, a := range chp {
			q[i] = shq(a)
		}
		pfx = "chpst " + strings.Join(q, " ") + " "
	}

	// ---- run ----
	var b strings.Builder
	b.WriteString(header(src))
	fmt.Fprintf(&b, "# Type=%s Restart=%s\n", typ, restart)
	b.WriteString("exec 2>&1\n")
	if restart == "no" && !(typ == "oneshot" && remain) {
		b.WriteString("# Restart=no: executa uma vez (sv once) — não reinicia ao sair\n")
		b.WriteString("printf o > supervise/control 2>/dev/null\n")
	}
	if len(req)+len(soft) > 0 {
		b.WriteString("\n# dependências (Requires/Wants/After)\n")
		for _, d := range req {
			fmt.Fprintf(&b, "sv check %s >/dev/null || exit 1\n", shq(filepath.Join(cfg.runDir, d)))
		}
		for _, d := range soft {
			fmt.Fprintf(&b, "[ -e %s ] && { sv check %s >/dev/null || exit 1; }\n", shq(filepath.Join(cfg.runDir, d)), shq(filepath.Join(cfg.runDir, d)))
		}
	}
	b.WriteString("\n")
	if v := g("UMask"); v != "" {
		fmt.Fprintf(&b, "umask %s\n", shq(v))
	}
	// ulimits que o chpst não cobre
	for k, flag := range map[string]string{"LimitCORE": "-c", "LimitMEMLOCK": "-l", "LimitSTACK": "-s", "LimitAS": "-v"} {
		if v := g(k); v != "" {
			v = strings.Split(v, ":")[0]
			if v == "infinity" {
				v = "unlimited"
			}
			fmt.Fprintf(&b, "ulimit %s %s\n", flag, shq(v))
		}
	}
	// diretórios gerenciados
	owner := ""
	if usr != "" {
		owner = " -o " + shq(usr)
		if grp != "" {
			owner += " -g " + shq(grp)
		}
	}
	for _, d := range []struct{ key, base, env string }{
		{"RuntimeDirectory", "/run", "RUNTIME_DIRECTORY"},
		{"StateDirectory", "/var/lib", "STATE_DIRECTORY"},
		{"CacheDirectory", "/var/cache", "CACHE_DIRECTORY"},
		{"LogsDirectory", "/var/log", "LOGS_DIRECTORY"},
		{"ConfigurationDirectory", "/etc", "CONFIGURATION_DIRECTORY"},
	} {
		var paths []string
		for _, w := range u.words("Service", d.key) {
			w = strings.SplitN(w, ":", 2)[0]
			p := filepath.Join(d.base, w)
			mode := g(d.key + "Mode")
			if mode == "" {
				mode = "0755"
			}
			fmt.Fprintf(&b, "install -d -m %s%s %s\n", shq(mode), owner, shq(p))
			paths = append(paths, p)
		}
		if len(paths) > 0 {
			fmt.Fprintf(&b, "export %s=%s\n", d.env, shq(strings.Join(paths, ":")))
		}
	}
	// ambiente
	for _, f := range u.all("Service", "EnvironmentFile") {
		opt := strings.HasPrefix(f, "-")
		f = strings.TrimPrefix(f, "-")
		if opt {
			fmt.Fprintf(&b, "[ -r %s ] && { set -a; . %s; set +a; }\n", shq(f), shq(f))
		} else {
			fmt.Fprintf(&b, "set -a; . %s || exit 1; set +a\n", shq(f))
		}
	}
	for _, kv := range u.words("Service", "Environment") {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			fmt.Fprintf(&b, "export %s=%s\n", k, shq(v))
		}
	}
	if wd := g("WorkingDirectory"); wd != "" {
		opt := strings.HasPrefix(wd, "-")
		wd = strings.TrimPrefix(wd, "-")
		target := shq(wd)
		if wd == "~" {
			target = `"$HOME"`
		}
		if opt {
			fmt.Fprintf(&b, "cd %s 2>/dev/null\n", target)
		} else {
			fmt.Fprintf(&b, "cd %s || exit 1\n", target)
		}
	}
	if rd := g("RootDirectory"); rd != "" {
		pfx = strings.Replace(pfx, "chpst ", "chpst -/ "+shq(rd)+" ", 1)
		if pfx == "" {
			pfx = "chpst -/ " + shq(rd) + " "
		}
	}
	switch out := g("StandardOutput"); {
	case out == "null":
		b.WriteString("exec >/dev/null\n")
	case strings.HasPrefix(out, "file:"):
		fmt.Fprintf(&b, "exec >%s\n", shq(out[5:]))
	case strings.HasPrefix(out, "append:"):
		fmt.Fprintf(&b, "exec >>%s\n", shq(out[7:]))
	}
	if g("StandardError") == "null" {
		b.WriteString("exec 2>/dev/null\n")
	}

	prePfx := pfx
	if parseBool(g("PermissionsStartOnly")) {
		prePfx = ""
	}
	pre := u.all("Service", "ExecStartPre")
	if len(pre) > 0 {
		b.WriteString("\n# ExecStartPre\n")
		for _, l := range pre {
			b.WriteString(parseExec(l).sh(prePfx, "exit 1"))
		}
	}
	post := u.all("Service", "ExecStartPost")

	b.WriteString("\n")
	switch typ {
	case "oneshot":
		for _, l := range starts {
			b.WriteString(parseExec(l).sh(pfx, "exit $?"))
		}
		for _, l := range post {
			b.WriteString(parseExec(l).sh(prePfx, "exit $?"))
		}
		if remain {
			b.WriteString("# RemainAfterExit=yes: permanece \"ativo\" até ser parado\n")
			b.WriteString("exec sleep infinity\n")
		} else {
			b.WriteString("exit 0\n")
		}
	case "forking":
		pidf := g("PIDFile")
		e := parseExec(starts[0])
		b.WriteString(e.sh(pfx, "exit 1"))
		fmt.Fprintf(&b, "i=0\nwhile [ ! -s %s ] && [ $i -lt 150 ]; do sleep 0.2; i=$((i+1)); done\n", shq(pidf))
		fmt.Fprintf(&b, "MAINPID=$(cat %s 2>/dev/null) || exit 1\n", shq(pidf))
		for _, l := range post {
			b.WriteString(parseExec(l).sh(prePfx, "true"))
		}
		b.WriteString("# Type=forking: o run fica vigiando o PID do daemon\n")
		b.WriteString("while kill -0 \"$MAINPID\" 2>/dev/null; do sleep 1; done\n")
		b.WriteString(wantUpFn)
		b.WriteString("want_up && exit 1 # morreu sozinho\nexit 0          # parado via voidctl stop\n")
	default:
		if len(post) > 0 {
			b.WriteString("# ExecStartPost (em segundo plano, após o início)\n(\n  sleep 1\n  MAINPID=$(cat supervise/pid 2>/dev/null)\n")
			for _, l := range post {
				b.WriteString("  " + parseExec(l).sh(prePfx, "true"))
			}
			b.WriteString(") &\n")
		}
		b.WriteString("exec " + pfx + parseExec(starts[0]).cmd + "\n")
	}
	r.files = append(r.files, genFile{"run", b.String(), 0755})

	// ---- finish ----
	var f strings.Builder
	f.WriteString(header(src))
	f.WriteString("# $1 = código de saída do run (-1 se morto por sinal), $2 = sinal\n")
	fmt.Fprintf(&f, "mkdir -p %s && echo \"$1 $2 $(date +%%s)\" > %s\n", shq(cfg.runtimeDir), shq(filepath.Join(cfg.runtimeDir, name+".exit")))
	f.WriteString("export EXIT_CODE=\"$1\" EXIT_STATUS=\"$2\"\n")
	for _, l := range u.all("Service", "ExecStopPost") {
		f.WriteString(parseExec(l).sh(prePfx, "true"))
	}
	if restart != "no" && restart != "always" {
		succ := []string{"0"}
		succSig := []string{"1", "2", "13", "15"}
		for _, w := range u.words("Service", "SuccessExitStatus") {
			if n, ok := sigNum(w); ok {
				succSig = append(succSig, fmt.Sprint(n))
			} else {
				succ = append(succ, w)
			}
		}
		fmt.Fprintf(&f, "clean() { case \"$1\" in %s) return 0;; -1) case \"$2\" in %s) return 0;; esac;; esac; return 1; }\n",
			strings.Join(succ, "|"), strings.Join(succSig, "|"))
		f.WriteString(wantUpFn)
		var cond string
		switch restart {
		case "on-success":
			cond = `clean "$1" "$2"`
		case "on-failure":
			cond = `! clean "$1" "$2"`
		case "on-abnormal", "on-abort":
			cond = `[ "$1" = -1 ] && ! clean "$1" "$2"`
		}
		fmt.Fprintf(&f, "# Restart=%s: deve reiniciar?\nshould_restart() { %s; }\nif want_up && ! should_restart \"$1\" \"$2\"; then\n  printf d > supervise/control\n  exit 0\nfi\n", restart, cond)
	}
	if rs := g("RestartSec"); rs != "" && restart != "no" {
		if d, err := parseTimespan(rs); err == nil && d >= time.Second {
			if !strings.Contains(f.String(), "want_up()") {
				f.WriteString(wantUpFn)
			}
			fmt.Fprintf(&f, "want_up && sleep %d\n", int(d.Seconds()))
		}
	}
	f.WriteString("exit 0\n")
	r.files = append(r.files, genFile{"finish", f.String(), 0755})

	// ---- control/t (parada) ----
	stops := u.all("Service", "ExecStop")
	ksig := strings.TrimPrefix(strings.ToUpper(g("KillSignal")), "SIG")
	if ksig == "" {
		ksig = "TERM"
	}
	if len(stops) > 0 || ksig != "TERM" || typ == "forking" {
		var t strings.Builder
		t.WriteString(header(src))
		t.WriteString("# chamado pelo runsv em 'sv down' / 'voidctl stop' no lugar do SIGTERM\n")
		if typ == "forking" {
			fmt.Fprintf(&t, "MAINPID=$(cat %s 2>/dev/null)\n", shq(g("PIDFile")))
		} else {
			t.WriteString("MAINPID=$(cat supervise/pid 2>/dev/null)\n")
		}
		for _, l := range stops {
			t.WriteString(parseExec(l).sh(prePfx, "true"))
		}
		fmt.Fprintf(&t, "[ -n \"$MAINPID\" ] && kill -%s \"$MAINPID\" 2>/dev/null && kill -CONT \"$MAINPID\" 2>/dev/null\nexit 0\n", ksig)
		r.files = append(r.files, genFile{"control/t", t.String(), 0755})
	}
	// ---- control/h (reload) ----
	if rl := u.all("Service", "ExecReload"); len(rl) > 0 {
		var h strings.Builder
		h.WriteString(header(src))
		h.WriteString("# chamado em 'sv hup' / 'voidctl reload'\nMAINPID=$(cat supervise/pid 2>/dev/null)\n")
		if typ == "forking" {
			fmt.Fprintf(&h, "MAINPID=$(cat %s 2>/dev/null)\n", shq(g("PIDFile")))
		}
		for _, l := range rl {
			h.WriteString(parseExec(l).sh(prePfx, "exit 1"))
		}
		h.WriteString("exit 0\n")
		r.files = append(r.files, genFile{"control/h", h.String(), 0755})
	}

	// ---- log/run ----
	logd := filepath.Join(cfg.logDir, name)
	r.files = append(r.files, genFile{"log/run",
		fmt.Sprintf("#!/bin/sh\n# Gerado por voidctl — logs lidos por 'voidctl logs -u %s'\nmkdir -p %s\nexec svlogd -tt %s\n", name, shq(logd), shq(logd)), 0755})

	// ---- metadados ----
	r.meta["source"] = src
	r.meta["unit"] = u.name
	r.meta["description"] = u.get("Unit", "Description")
	r.meta["type"] = typ
	r.meta["restart"] = restart
	r.meta["remain"] = map[bool]string{true: "yes", false: "no"}[remain]
	r.meta["requires"] = strings.Join(req, " ")
	r.meta["wants"] = strings.Join(wants, " ")
	r.meta["after"] = strings.Join(after, " ")
	r.meta["user"] = usr
	if typ == "forking" {
		r.meta["pidfile"] = g("PIDFile")
	}
	r.meta["generated"] = time.Now().Format(time.RFC3339)
	if ts := g("TimeoutStopSec"); ts != "" {
		if d, err := parseTimespan(ts); err == nil {
			r.meta["timeoutstop"] = fmt.Sprint(int(d.Seconds()))
		}
	}
	if ts := g("TimeoutStartSec"); ts != "" {
		if d, err := parseTimespan(ts); err == nil {
			r.meta["timeoutstart"] = fmt.Sprint(int(d.Seconds()))
		}
	}
	if len(u.sources) > 1 {
		r.meta["dropins"] = strings.Join(u.sources[1:], " ")
	}
	if o.timer != "" {
		r.meta["timer"] = o.timer
		r.down = true // só sobe quando o timer disparar
	}
	r.raw = u.raw.String()
	return r, nil
}

func sigNum(s string) (int, bool) {
	s = strings.TrimPrefix(strings.ToUpper(s), "SIG")
	for n, name := range sigNames {
		if name == s {
			return n, true
		}
	}
	return 0, false
}

// installService grava os arquivos gerados em /etc/sv/NOME.
func installService(r *genResult, force bool) (changed bool, err error) {
	dir := svDefDir(r.name)
	if exists(dir) && !exists(filepath.Join(dir, "voidctl.meta")) && !force {
		return false, fmt.Errorf("%s já existe e não foi gerado pelo voidctl (use --force para sobrescrever)", dir)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return false, err
	}
	want := map[string]bool{}
	for _, f := range r.files {
		want[f.rel] = true
		p := filepath.Join(dir, f.rel)
		old, _ := os.ReadFile(p)
		if string(old) == f.content {
			continue
		}
		changed = true
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			return changed, err
		}
		if err := atomicWrite(p, []byte(f.content), f.mode); err != nil {
			return changed, err
		}
	}
	// remove scripts gerados anteriormente que não existem mais
	for _, rel := range []string{"control/t", "control/h", "finish", "log/run"} {
		p := filepath.Join(dir, rel)
		if !want[rel] && exists(p) {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), "Gerado por voidctl") {
				os.Remove(p)
				changed = true
			}
		}
	}
	// na convenção do Void, supervise fica em /run/runit
	if isDir("/run/runit") {
		for _, s := range []struct{ link, target string }{
			{filepath.Join(dir, "supervise"), "/run/runit/supervise." + r.name},
			{filepath.Join(dir, "log", "supervise"), "/run/runit/supervise." + r.name + "-log"},
		} {
			if !lexists(s.link) && isDir(filepath.Dir(s.link)) {
				os.Symlink(s.target, s.link)
			}
		}
	}
	downf := filepath.Join(dir, "down")
	if r.down && !exists(downf) {
		os.WriteFile(downf, nil, 0644)
	}
	oldMeta := readMeta(r.name)
	if oldMeta != nil {
		// preserva o vínculo com timer se a reimportação não o informou
		if r.meta["timer"] == "" && oldMeta["timer"] != "" {
			r.meta["timer"] = oldMeta["timer"]
		}
		cmp := func(m map[string]string) string {
			c := map[string]string{}
			for k, v := range m {
				if k != "generated" {
					c[k] = v
				}
			}
			return writeMeta("", c)
		}
		if cmp(oldMeta) == cmp(r.meta) && !changed {
			return false, nil
		}
	}
	changed = true
	if err := atomicWrite(metaPath(r.name), []byte(writeMeta("", r.meta)), 0644); err != nil {
		return changed, err
	}
	atomicWrite(filepath.Join(dir, "voidctl.service"), []byte(r.raw), 0644)
	return changed, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".voidctl-tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	os.Chmod(tmp, mode)
	return os.Rename(tmp, path)
}

func printGenerated(r *genResult) {
	dir := svDefDir(r.name)
	for _, f := range r.files {
		fmt.Printf("%s# %s%s\n%s\n", cyan, filepath.Join(dir, f.rel), reset, f.content)
	}
	if r.down {
		fmt.Printf("%s# %s%s  (arquivo vazio: não sobe sozinho no boot)\n\n", cyan, filepath.Join(dir, "down"), reset)
	}
	fmt.Printf("%s# %s%s\n%s\n", cyan, metaPath(r.name), reset, writeMeta("", r.meta))
}

// ---------- comando import ----------

func cmdImport(args []string) int {
	o, err := parseArgs(args, map[string]fdef{
		"--dry-run": {"dry", false}, "-n": {"dry", false},
		"--force": {"force", false}, "-f": {"force", false},
		"--enable": {"enable", false}, "--now": {"now", false},
		"--restart": {"restart", true}, "--name": {"name", true},
	})
	if err != nil {
		errorf("%v", err)
		return 2
	}
	if len(o.pos) == 0 {
		errorf("uso: voidctl import [--dry-run] [--force] [--enable] [--now] [--restart=POL] [--name=N] UNIT...")
		return 2
	}
	if o.str("name") != "" && len(o.pos) > 1 {
		errorf("--name só pode ser usado com uma unit")
		return 2
	}
	rc := 0
	for _, arg := range o.pos {
		path, uname, err := findUnit(arg)
		if err != nil {
			errorf("%v", err)
			rc = 1
			continue
		}
		io := importOpts{name: o.str("name"), restart: o.str("restart"), force: o.bool("force"), dryRun: o.bool("dry")}
		switch filepath.Ext(uname) {
		case ".service":
			name, err := importService(path, uname, io)
			if err != nil {
				errorf("%v", err)
				rc = 1
				continue
			}
			if !io.dryRun && (o.bool("enable") || o.bool("now")) {
				if err := enableUnit(name, o.bool("now")); err != nil {
					errorf("%v", err)
					rc = 1
				}
			}
		case ".timer":
			base, err := importTimer(path, uname, io)
			if err != nil {
				errorf("%v", err)
				rc = 1
				continue
			}
			if !io.dryRun && (o.bool("enable") || o.bool("now")) {
				if err := enableTimer(base); err != nil {
					errorf("%v", err)
					rc = 1
				}
			}
		case ".socket":
			errorf("%s: ativação por socket não existe no runit; importe o .service correspondente (o daemon precisa abrir o socket sozinho)", uname)
			rc = 1
		default:
			errorf("%s: tipo de unit não suportado (use .service ou .timer)", uname)
			rc = 1
		}
	}
	return rc
}

func importService(path, uname string, io importOpts) (string, error) {
	u, err := loadUnit(path, uname)
	if err != nil {
		return "", err
	}
	if io.timer == "" {
		name := io.name
		if name == "" {
			name = u.base()
		}
		if m := readMeta(name); m != nil {
			io.timer = m["timer"]
		}
	}
	r, err := convertService(u, io)
	if err != nil {
		return "", err
	}
	for _, w := range r.warns {
		warnf("%s: %s", uname, w)
	}
	if io.dryRun {
		printGenerated(r)
		return r.name, nil
	}
	changed, err := installService(r, io.force)
	if err != nil {
		return "", err
	}
	if changed {
		notef("%s → %s", path, svDefDir(r.name))
		if readStatus(r.name).state == stRun {
			notef("  %s está rodando; aplique com: voidctl restart %s", r.name, r.name)
		}
	}
	return r.name, nil
}

// ---------- daemon-reload ----------

func cmdDaemonReload(args []string) int {
	rc := 0
	ents, _ := os.ReadDir(cfg.svDir)
	names := []string{}
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	n := 0
	for _, name := range names {
		m := readMeta(name)
		if m == nil || m["source"] == "" {
			continue
		}
		src := m["source"]
		// uma cópia em /etc/voidctl/system tem precedência
		if p := filepath.Join(cfg.unitDir, filepath.Base(src)); exists(p) {
			src = p
		}
		if !exists(src) {
			warnf("%s: unit de origem %s não existe mais", name, src)
			continue
		}
		if _, err := importService(src, m["unit"], importOpts{name: name, timer: m["timer"]}); err != nil {
			errorf("%v", err)
			rc = 1
		}
		n++
	}
	// timers
	tents, _ := filepath.Glob(filepath.Join(cfg.timerDir, "*.timer"))
	for _, t := range tents {
		if _, err := loadTimerDef(t); err != nil {
			errorf("%v", err)
			rc = 1
		}
	}
	if readStatus(timerdName).supervised {
		svControl(timerdName, "h")
	}
	notef("%d serviço(s) importado(s) verificados, %d timer(s).", n, len(tents))
	return rc
}

// ======================================================================
// comandos estilo systemctl
// ======================================================================
var noBlock bool

func unitArg(s string) (name, kind string) {
	switch {
	case strings.HasSuffix(s, ".service"):
		return strings.TrimSuffix(s, ".service"), "service"
	case strings.HasSuffix(s, ".timer"):
		return strings.TrimSuffix(s, ".timer"), "timer"
	}
	return strings.TrimSuffix(s, "/"), "service"
}

func mustExist(name string) error {
	if !serviceExists(name) {
		return fmt.Errorf("Unit %s.service não encontrada (nem em %s nem em %s).", name, cfg.svDir, cfg.runDir)
	}
	return nil
}

// ensureSupervised garante que o runsvdir supervisione o serviço. Se ele
// não estiver habilitado, cria um link "de runtime" com arquivo down, para
// que não suba sozinho no próximo boot (semântica do systemctl start).
func ensureSupervised(name string) error {
	if !isLinked(name) {
		downf := filepath.Join(svDefDir(name), "down")
		content := ""
		if !exists(downf) {
			if err := os.WriteFile(downf, nil, 0644); err != nil {
				return err
			}
			content = "down"
		}
		os.MkdirAll(filepath.Dir(runtimeMarker(name)), 0755)
		if err := os.WriteFile(runtimeMarker(name), []byte(content), 0644); err != nil {
			return err
		}
		if err := os.Symlink(svDefDir(name), svLink(name)); err != nil {
			return err
		}
	}
	if readStatus(name).supervised {
		return nil
	}
	// runsvdir varre o diretório a cada 5 s
	if _, ok := waitStatus(name, 12*time.Second, func(s svStatus) bool { return s.supervised }); !ok {
		return fmt.Errorf("%s: runsvdir não assumiu o serviço (o runsvdir está rodando em %s?)", name, cfg.runDir)
	}
	return nil
}

// cleanupRuntime remove o link temporário criado por start sem enable.
func cleanupRuntime(name string) {
	b, err := os.ReadFile(runtimeMarker(name))
	if err != nil {
		return
	}
	os.Remove(svLink(name))
	if string(b) == "down" {
		os.Remove(filepath.Join(svDefDir(name), "down"))
	}
	os.Remove(runtimeMarker(name))
}

func startUnit(name string, visited map[string]bool) error {
	if visited[name] {
		return nil
	}
	visited[name] = true
	if err := mustExist(name); err != nil {
		return err
	}
	if isMasked(name) {
		return fmt.Errorf("Unit %s.service está mascarada.", name)
	}
	req, wants := depsOf(name)
	for _, d := range req {
		if err := startUnit(d, visited); err != nil {
			return fmt.Errorf("dependência %s de %s falhou: %v", d, name, err)
		}
	}
	for _, d := range wants {
		if serviceExists(d) && !isMasked(d) {
			if err := startUnit(d, visited); err != nil {
				warnf("dependência opcional %s: %v", d, err)
			}
		}
	}
	if err := ensureSupervised(name); err != nil {
		return err
	}
	st := readStatus(name)
	if st.state == stRun && st.want == 'u' {
		return nil
	}
	t0 := time.Now()
	if err := svControl(name, "u"); err != nil {
		return err
	}
	if noBlock {
		return nil
	}
	return waitStarted(name, t0, st)
}

func waitStarted(name string, t0 time.Time, before svStatus) error {
	m := readMeta(name)
	fresh := func(s svStatus) bool { return s.since.After(t0.Add(-time.Second)) && s.since != before.since }
	if m != nil && m["type"] == "oneshot" && m["remain"] != "yes" {
		// oneshot: espera terminar, como o systemctl faz
		timeout := 5 * time.Minute
		if v, err := strconv.Atoi(m["timeoutstart"]); err == nil && v > 0 {
			timeout = time.Duration(v) * time.Second
		}
		st, ok := waitStatus(name, timeout, func(s svStatus) bool { return s.state == stDown && fresh(s) })
		if !ok {
			return fmt.Errorf("%s: tempo esgotado esperando o oneshot terminar (estado %d)", name, st.state)
		}
		if e := readExit(name); e.ok && !e.clean() {
			return fmt.Errorf("Job para %s.service falhou (%s). Veja: voidctl status %s", name, e, name)
		}
		return nil
	}
	timeout := cfg.wait
	if m != nil {
		if v, err := strconv.Atoi(m["timeoutstart"]); err == nil && v > 0 {
			timeout = time.Duration(v) * time.Second
		}
	}
	st, ok := waitStatus(name, timeout, func(s svStatus) bool { return s.state == stRun && s.pid > 0 && fresh(s) })
	if !ok {
		return fmt.Errorf("Job para %s.service falhou: não subiu em %s. Veja: voidctl status %s", name, timeout, name)
	}
	// script check (como sv check)
	if chk := filepath.Join(svPath(name), "check"); exists(chk) {
		deadline := time.Now().Add(timeout)
		for {
			c := exec.Command(chk)
			c.Dir = svPath(name)
			if c.Run() == nil {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s: script check não confirmou o serviço em %s", name, timeout)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	time.Sleep(300 * time.Millisecond)
	now := readStatus(name)
	if now.pid != st.pid || now.state != stRun {
		return fmt.Errorf("Job para %s.service falhou: o processo saiu logo após iniciar. Veja: voidctl status %s", name, name)
	}
	return nil
}

func stopUnit(name string, visited map[string]bool) error {
	if visited[name] {
		return nil
	}
	visited[name] = true
	if err := mustExist(name); err != nil {
		return err
	}
	for _, d := range reverseRequires(name) {
		if s := readStatus(d); s.state != stDown {
			notef("parando %s (depende de %s)", d, name)
			if err := stopUnit(d, visited); err != nil {
				warnf("%v", err)
			}
		}
	}
	st := readStatus(name)
	if !st.supervised {
		cleanupRuntime(name)
		return nil
	}
	if err := svControl(name, "d"); err != nil {
		return err
	}
	if noBlock {
		return nil
	}
	timeout := cfg.wait
	if m := readMeta(name); m != nil {
		if v, err := strconv.Atoi(m["timeoutstop"]); err == nil && v > 0 {
			timeout = time.Duration(v) * time.Second
		}
	}
	if _, ok := waitStatus(name, timeout, func(s svStatus) bool { return s.state == stDown }); !ok {
		warnf("%s não parou em %s; enviando SIGKILL", name, timeout)
		svControl(name, "k")
		if _, ok := waitStatus(name, 3*time.Second, func(s svStatus) bool { return s.state == stDown }); !ok {
			return fmt.Errorf("%s: não foi possível parar", name)
		}
	}
	cleanupRuntime(name)
	return nil
}

func restartUnit(name string, onlyIfRunning bool) error {
	if err := mustExist(name); err != nil {
		return err
	}
	st := readStatus(name)
	if st.state != stRun {
		if onlyIfRunning {
			return nil
		}
		return startUnit(name, map[string]bool{})
	}
	t0 := time.Now()
	if err := svControl(name, "tcu"); err != nil {
		return err
	}
	if noBlock {
		return nil
	}
	return waitStarted(name, t0, st)
}

func enableUnit(name string, now bool) error {
	if err := mustExist(name); err != nil {
		return err
	}
	if isMasked(name) {
		return fmt.Errorf("Unit %s.service está mascarada; use 'voidctl unmask %s'", name, name)
	}
	if isRuntime(name) {
		b, _ := os.ReadFile(runtimeMarker(name))
		if string(b) == "down" {
			os.Remove(filepath.Join(svDefDir(name), "down"))
		}
		os.Remove(runtimeMarker(name))
		notef("%s agora sobe no boot.", name)
	} else if !isLinked(name) {
		if err := os.Symlink(svDefDir(name), svLink(name)); err != nil {
			return err
		}
		notef("Created symlink %s → %s.", svLink(name), svDefDir(name))
	}
	if exists(filepath.Join(svDefDir(name), "down")) {
		if m := readMeta(name); m == nil || m["timer"] == "" {
			warnf("%s possui o arquivo %s/down: runsv não o inicia sozinho no boot", name, svDefDir(name))
		}
	}
	// dependências obrigatórias precisam estar supervisionadas no boot
	req, _ := depsOf(name)
	for _, d := range req {
		if serviceExists(d) && (!isLinked(d) || isRuntime(d)) && !isMasked(d) {
			notef("habilitando dependência %s", d)
			if err := enableUnit(d, false); err != nil {
				warnf("%v", err)
			}
		} else if !serviceExists(d) {
			warnf("%s depende de %s, que não existe em %s", name, d, cfg.svDir)
		}
	}
	if now {
		return startUnit(name, map[string]bool{})
	}
	return nil
}

func disableUnit(name string, now bool) error {
	if err := mustExist(name); err != nil {
		return err
	}
	if !isLinked(name) {
		return nil
	}
	if now {
		if err := stopUnit(name, map[string]bool{}); err != nil {
			return err
		}
	}
	if !isLinked(name) { // stopUnit pode ter limpado link de runtime
		return nil
	}
	if !now && readStatus(name).state != stDown && !isRuntime(name) {
		// systemctl disable não para o serviço: vira "runtime"
		downf := filepath.Join(svDefDir(name), "down")
		content := ""
		if !exists(downf) {
			os.WriteFile(downf, nil, 0644)
			content = "down"
		}
		os.MkdirAll(filepath.Dir(runtimeMarker(name)), 0755)
		os.WriteFile(runtimeMarker(name), []byte(content), 0644)
		notef("%s não subirá mais no boot (continua rodando agora; use --now para parar).", name)
		return nil
	}
	if isRuntime(name) {
		cleanupRuntime(name)
	} else {
		os.Remove(svLink(name))
	}
	notef("Removed %s.", svLink(name))
	return nil
}

func maskUnit(name string) error {
	if err := mustExist(name); err != nil {
		return err
	}
	os.MkdirAll(cfg.maskDir, 0755)
	if !lexists(filepath.Join(cfg.maskDir, name)) {
		if err := os.Symlink("/dev/null", filepath.Join(cfg.maskDir, name)); err != nil {
			return err
		}
	}
	notef("Created symlink %s → /dev/null.", filepath.Join(cfg.maskDir, name))
	return disableUnit(name, false)
}

func unmaskUnit(name string) error {
	p := filepath.Join(cfg.maskDir, name)
	if lexists(p) {
		os.Remove(p)
		notef("Removed %s.", p)
	}
	return nil
}

var killLetters = map[string]string{
	"TERM": "t", "HUP": "h", "INT": "i", "QUIT": "q", "USR1": "1", "USR2": "2",
	"ALRM": "a", "KILL": "k", "CONT": "c", "STOP": "p",
}

func killUnit(name, sig string) error {
	if err := mustExist(name); err != nil {
		return err
	}
	sig = strings.TrimPrefix(strings.ToUpper(sig), "SIG")
	if n, err := strconv.Atoi(sig); err == nil {
		sig = sigName(n)
	}
	if l, ok := killLetters[sig]; ok {
		return svControl(name, l)
	}
	n, ok := sigNum(sig)
	if !ok {
		return fmt.Errorf("sinal desconhecido: %s", sig)
	}
	st := readStatus(name)
	if st.pid == 0 {
		return fmt.Errorf("%s não está rodando", name)
	}
	return syscall.Kill(st.pid, syscall.Signal(n))
}

// ---------- comandos de consulta ----------

func cmdStatus(args []string) int {
	o, err := parseArgs(args, map[string]fdef{"-n": {"lines", true}, "--lines": {"lines", true}, "--no-pager": {"np", false}, "-l": {"full", false}, "--full": {"full", false}})
	if err != nil {
		errorf("%v", err)
		return 2
	}
	lines, _ := o.int("lines", 10)
	if len(o.pos) == 0 {
		return statusOverview()
	}
	rc := 0
	for i, a := range o.pos {
		name, kind := unitArg(a)
		if i > 0 {
			fmt.Println()
		}
		if kind == "timer" {
			if statusTimer(name) != nil {
				rc = 4
			}
			continue
		}
		if !serviceExists(name) {
			errorf("Unit %s.service could not be found.", name)
			rc = 4
			continue
		}
		u := getState(name)
		printStatus(u, lines)
		if u.active != "active" && rc == 0 {
			rc = 3
		}
	}
	return rc
}

func printStatus(u unitState, lines int) {
	fmt.Printf("%s %s%s.service%s - %s\n", u.dot(), bold, u.name, reset, u.description())
	loaded := fmt.Sprintf("loaded (%s; %s", svDefDir(u.name), u.enabled)
	if !u.normalUp {
		loaded += "; down"
	}
	if u.meta != nil {
		loaded += "; origem: " + u.meta["source"]
	}
	fmt.Printf("     Loaded: %s)\n", loaded)
	if u.meta != nil && u.meta["dropins"] != "" {
		fmt.Printf("    Drop-In: %s\n", strings.ReplaceAll(u.meta["dropins"], " ", "\n             "))
	}
	if u.meta != nil && u.meta["timer"] != "" {
		fmt.Printf("TriggeredBy: %s.timer\n", u.meta["timer"])
	}
	act := fmt.Sprintf("%s (%s)", u.activeColored(), u.sub)
	if !u.st.since.IsZero() && u.st.supervised {
		act += fmt.Sprintf(" since %s; %s ago", fmtStamp(u.st.since), fmtSpan(time.Since(u.st.since)))
	} else if u.exit.ok {
		act += fmt.Sprintf(" since %s; %s ago", fmtStamp(u.exit.at), fmtSpan(time.Since(u.exit.at)))
	}
	fmt.Printf("     Active: %s\n", act)
	if !u.st.supervised && isLinked(u.name) {
		fmt.Printf("     %sAviso:%s link em %s existe mas nenhum runsv responde\n", yellow, reset, cfg.runDir)
	}
	if u.exit.ok && u.st.state != stRun {
		col := ""
		if !u.exit.clean() {
			col = red
		}
		fmt.Printf("    Process: %s(última saída em %s: %s)%s\n", col, u.exit.at.Local().Format("15:04:05"), u.exit, reset)
	}
	if u.st.state == stRun && u.st.pid > 0 {
		root := u.st.pid
		mainPID := u.st.pid
		if u.meta != nil && u.meta["pidfile"] != "" { // Type=forking: o PID real está no PIDFile
			if b, err := os.ReadFile(u.meta["pidfile"]); err == nil {
				if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n > 0 {
					mainPID = n
				}
			}
		}
		fmt.Printf("   Main PID: %d (%s)\n", mainPID, procComm(mainPID))
		procs := listProcs()
		pids, depth := procTree(root, procs)
		if mainPID != root {
			extra, d2 := procTree(mainPID, procs)
			for _, p := range extra {
				depth[p] = d2[p]
			}
			pids = append(pids, extra...)
		}
		tasks := 0
		var mem int64
		for _, p := range pids {
			tasks += procTasks(p)
			mem += procRSS(p)
		}
		fmt.Printf("      Tasks: %d\n", tasks)
		fmt.Printf("     Memory: %s\n", fmtBytes(mem))
		fmt.Printf("  Processes: %s\n", svPath(u.name))
		for i, p := range pids {
			// último irmão: nenhum pid seguinte na mesma profundidade antes de subir
			last := true
			for j := i + 1; j < len(pids) && depth[pids[j]] >= depth[p]; j++ {
				if depth[pids[j]] == depth[p] {
					last = false
					break
				}
			}
			branch := "├─"
			if last {
				branch = "└─"
			}
			fmt.Printf("             %s%s%d %s\n", strings.Repeat("  ", depth[p]), gray+branch+reset, p, procCmdline(p))
		}
	}
	if lines > 0 {
		entries := collectLogs(unitLogSources(u.name), logFilter{})
		if len(entries) > lines {
			entries = entries[len(entries)-lines:]
		}
		if len(entries) > 0 {
			fmt.Println()
			host, _ := os.Hostname()
			for _, e := range entries {
				fmt.Println(formatEntry(e, "short", host))
			}
		}
	}
}

func statusOverview() int {
	host, _ := os.Hostname()
	names := listServiceNames(false)
	var failed []string
	running := 0
	for _, n := range names {
		u := getState(n)
		switch u.active {
		case "failed", "activating":
			failed = append(failed, n)
		case "active":
			running++
		}
	}
	state := green + "running" + reset
	if len(failed) > 0 {
		state = yellow + "degraded" + reset
	}
	fmt.Printf("%s● %s%s\n", green, reset, bold+host+reset)
	fmt.Printf("    State: %s\n", state)
	fmt.Printf("    Units: %d supervisionados, %d ativos, %d com falha\n", len(names), running, len(failed))
	fmt.Printf("     Boot: %s (%s ago)\n", fmtStamp(bootTime()), fmtSpan(time.Since(bootTime())))
	if len(failed) > 0 {
		fmt.Printf("   Falhas: %s%s%s\n", red, strings.Join(failed, " "), reset)
	}
	return 0
}

func cmdListUnits(args []string) int {
	o, err := parseArgs(args, map[string]fdef{"-a": {"all", false}, "--all": {"all", false}, "--failed": {"failed", false}, "--state": {"state", true}, "--no-legend": {"nolegend", false}, "--no-pager": {"np", false}, "--type": {"type", true}, "-t": {"type", true}})
	if err != nil {
		errorf("%v", err)
		return 2
	}
	if o.str("type") == "timer" {
		return cmdListTimers(nil)
	}
	var rows [][]string
	for _, n := range listServiceNames(o.bool("all")) {
		u := getState(n)
		if o.bool("failed") && u.active != "failed" {
			continue
		}
		if s := o.str("state"); s != "" && s != u.active && s != u.sub {
			continue
		}
		name := n + ".service"
		if u.active == "failed" {
			name = red + name + reset
		}
		act, sub := u.active, u.sub
		switch u.active {
		case "active":
			act, sub = green+act+reset, green+sub+reset
		case "failed":
			act, sub = red+act+reset, red+sub+reset
		}
		rows = append(rows, []string{u.dot() + " " + name, "loaded", act, sub, u.description()})
	}
	if o.bool("nolegend") {
		for _, r := range rows {
			fmt.Println(strings.Join(r, " "))
		}
		return 0
	}
	printTable([]string{"  UNIT", "LOAD", "ACTIVE", "SUB", "DESCRIPTION"}, rows)
	fmt.Printf("\n%d units listadas.", len(rows))
	if !o.bool("all") {
		fmt.Printf(" Use --all para ver também os serviços não habilitados.")
	}
	fmt.Println()
	return 0
}

func cmdListUnitFiles(args []string) int {
	var rows [][]string
	for _, n := range listServiceNames(true) {
		u := getState(n)
		st := u.enabled
		switch st {
		case "enabled":
			st = green + st + reset
		case "masked":
			st = red + st + reset
		}
		origin := "runit"
		if u.meta != nil {
			origin = "voidctl (" + filepath.Base(u.meta["source"]) + ")"
		}
		rows = append(rows, []string{n + ".service", st, origin})
	}
	for _, t := range loadAllTimers() {
		st := "disabled"
		if t.enabled {
			st = green + "enabled" + reset
		}
		rows = append(rows, []string{t.name + ".timer", st, "voidctl"})
	}
	printTable([]string{"UNIT FILE", "STATE", "ORIGEM"}, rows)
	fmt.Printf("\n%d unit files listadas.\n", len(rows))
	return 0
}

func cmdIs(which string, args []string) int {
	if len(args) == 0 {
		errorf("uso: voidctl %s NOME...", which)
		return 2
	}
	rc := 1
	if which == "is-active" || which == "is-failed" {
		rc = 3
	}
	anyOK := false
	for _, a := range args {
		name, kind := unitArg(a)
		var out string
		ok := false
		if kind == "timer" {
			t, err := loadTimerDef(filepath.Join(cfg.timerDir, name+".timer"))
			switch {
			case err != nil:
				out = "not-found"
			case which == "is-enabled":
				out, ok = map[bool]string{true: "enabled", false: "disabled"}[t.enabled], t.enabled
			case which == "is-active":
				out, ok = map[bool]string{true: "active", false: "inactive"}[t.enabled], t.enabled
			default:
				out = "inactive"
			}
		} else {
			u := getState(name)
			switch which {
			case "is-active":
				out, ok = u.active, u.active == "active"
			case "is-enabled":
				out, ok = u.enabled, u.enabled == "enabled" || u.enabled == "static"
			case "is-failed":
				out, ok = u.active, u.active == "failed"
			}
		}
		if !quiet {
			fmt.Println(out)
		}
		if ok {
			anyOK = true
		}
	}
	if anyOK {
		return 0
	}
	return rc
}

func cmdListDeps(args []string) int {
	o, err := parseArgs(args, map[string]fdef{"--reverse": {"rev", false}, "--all": {"all", false}, "--no-pager": {"np", false}})
	if err != nil || len(o.pos) != 1 {
		errorf("uso: voidctl list-dependencies [--reverse] NOME")
		return 2
	}
	name, _ := unitArg(o.pos[0])
	if err := mustExist(name); err != nil {
		errorf("%v", err)
		return 1
	}
	fmt.Printf("%s.service\n", name)
	var walk func(n, indent string, seen map[string]bool)
	walk = func(n, indent string, seen map[string]bool) {
		var kids []string
		if o.bool("rev") {
			kids = reverseRequires(n)
		} else {
			req, wants := depsOf(n)
			kids = append(req, wants...)
		}
		for i, k := range kids {
			br, next := "├─", "│ "
			if i == len(kids)-1 {
				br, next = "└─", "  "
			}
			u := getState(k)
			fmt.Printf("%s%s%s %s.service\n", indent, br, u.dot(), k)
			if !seen[k] {
				s2 := map[string]bool{k: true}
				for kk := range seen {
					s2[kk] = true
				}
				walk(k, indent+next, s2)
			}
		}
	}
	walk(name, "", map[string]bool{name: true})
	return 0
}

func cmdCat(args []string) int {
	rc := 0
	for _, a := range args {
		name, kind := unitArg(a)
		if kind == "timer" {
			p := filepath.Join(cfg.timerDir, name+".timer")
			b, err := os.ReadFile(p)
			if err != nil {
				errorf("%v", err)
				rc = 1
				continue
			}
			fmt.Printf("%s# %s%s\n%s\n", cyan, p, reset, b)
			continue
		}
		if err := mustExist(name); err != nil {
			errorf("%v", err)
			rc = 1
			continue
		}
		dir := svDefDir(name)
		if b, err := os.ReadFile(filepath.Join(dir, "voidctl.service")); err == nil {
			fmt.Printf("%s%s%s\n", gray, strings.TrimRight(string(b), "\n"), reset)
			fmt.Println()
		}
		for _, rel := range []string{"run", "finish", "check", "conf", "control/t", "control/h", "control/d", "control/u", "log/run"} {
			p := filepath.Join(dir, rel)
			if b, err := os.ReadFile(p); err == nil {
				fmt.Printf("%s# %s%s\n%s\n", cyan, p, reset, strings.TrimRight(string(b), "\n"))
				fmt.Println()
			}
		}
	}
	return rc
}

func cmdShow(args []string) int {
	o, err := parseArgs(args, map[string]fdef{"-p": {"prop", true}, "--property": {"prop", true}, "--value": {"value", false}})
	if err != nil || len(o.pos) == 0 {
		errorf("uso: voidctl show [-p PROP[,PROP]] [--value] NOME...")
		return 2
	}
	var want map[string]bool
	for _, p := range o.strs("prop") {
		if want == nil {
			want = map[string]bool{}
		}
		for _, q := range strings.Split(p, ",") {
			want[q] = true
		}
	}
	for i, a := range o.pos {
		name, _ := unitArg(a)
		u := getState(name)
		if i > 0 {
			fmt.Println()
		}
		ts := func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return fmtStamp(t)
		}
		props := [][2]string{
			{"Id", name + ".service"},
			{"Description", u.description()},
			{"LoadState", map[bool]string{true: "loaded", false: "not-found"}[u.exists]},
			{"ActiveState", u.active},
			{"SubState", u.sub},
			{"UnitFileState", u.enabled},
			{"MainPID", strconv.Itoa(u.st.pid)},
			{"ActiveEnterTimestamp", ts(u.st.since)},
			{"FragmentPath", svDefDir(name)},
			{"NormallyUp", map[bool]string{true: "yes", false: "no"}[u.normalUp]},
		}
		if u.exit.ok {
			props = append(props, [2]string{"ExecMainStatus", strconv.Itoa(u.exit.code)}, [2]string{"ExecMainExitTimestamp", ts(u.exit.at)})
		}
		if u.meta != nil {
			props = append(props,
				[2]string{"SourcePath", u.meta["source"]},
				[2]string{"Type", u.meta["type"]},
				[2]string{"Restart", u.meta["restart"]},
				[2]string{"Requires", u.meta["requires"]},
				[2]string{"Wants", u.meta["wants"]},
				[2]string{"After", u.meta["after"]},
				[2]string{"User", u.meta["user"]},
				[2]string{"TriggeredBy", u.meta["timer"]})
		}
		for _, p := range props {
			if want != nil && !want[p[0]] {
				continue
			}
			if o.bool("value") {
				fmt.Println(p[1])
			} else {
				fmt.Printf("%s=%s\n", p[0], p[1])
			}
		}
	}
	return 0
}

func cmdEdit(args []string) int {
	if len(args) != 1 {
		errorf("uso: voidctl edit NOME")
		return 2
	}
	name, kind := unitArg(args[0])
	editor := getenv("VISUAL", getenv("EDITOR", "vi"))
	run := func(p string) error {
		c := exec.Command("sh", "-c", editor+` "$1"`, "editor", p)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		return c.Run()
	}
	if kind == "timer" {
		p := filepath.Join(cfg.timerDir, name+".timer")
		if err := run(p); err != nil {
			errorf("%v", err)
			return 1
		}
		if _, err := loadTimerDef(p); err != nil {
			errorf("%v", err)
			return 1
		}
		svControl(timerdName, "h")
		return 0
	}
	if err := mustExist(name); err != nil {
		errorf("%v", err)
		return 1
	}
	m := readMeta(name)
	if m == nil {
		if err := run(filepath.Join(svDefDir(name), "run")); err != nil {
			errorf("%v", err)
			return 1
		}
		return 0
	}
	// copia a unit para /etc/voidctl/system para ser editada ali
	dst := filepath.Join(cfg.unitDir, filepath.Base(m["source"]))
	if !exists(dst) {
		b, err := os.ReadFile(m["source"])
		if err != nil {
			errorf("%v", err)
			return 1
		}
		os.MkdirAll(cfg.unitDir, 0755)
		if err := os.WriteFile(dst, b, 0644); err != nil {
			errorf("%v", err)
			return 1
		}
	}
	if err := run(dst); err != nil {
		errorf("%v", err)
		return 1
	}
	if _, err := importService(dst, m["unit"], importOpts{name: name, timer: m["timer"], force: true}); err != nil {
		errorf("%v", err)
		return 1
	}
	return 0
}

const bootHook = "/etc/runit/core-services/99-voidctl.sh"

// cmdSetup instala o que o voidctl precisa no sistema: o hook do estágio 1
// (limpa serviços iniciados sem enable) e os diretórios de configuração.
func cmdSetup() int {
	self, err := os.Executable()
	if err != nil {
		self = "/usr/bin/voidctl"
	}
	hook := fmt.Sprintf("# Gerado por 'voidctl setup' — remove links de serviços iniciados sem enable\n[ -x %s ] && %s -q boot-cleanup\n", shq(self), shq(self))
	if isDir(filepath.Dir(bootHook)) {
		if err := os.WriteFile(bootHook, []byte(hook), 0644); err != nil {
			errorf("%v", err)
			return 1
		}
		notef("hook de boot: %s", bootHook)
	} else {
		warnf("%s não existe (não é um sistema runit do Void?); hook não instalado", filepath.Dir(bootHook))
	}
	for _, d := range []string{cfg.unitDir, filepath.Join(cfg.timerDir, "enabled"), cfg.maskDir, filepath.Join(cfg.stateDir, "runtime"), filepath.Join(cfg.stateDir, "timers")} {
		if err := os.MkdirAll(d, 0755); err != nil {
			errorf("%v", err)
			return 1
		}
	}
	notef("diretórios criados em %s e %s", cfg.confDir, cfg.stateDir)
	return 0
}

// cmdBootCleanup roda no estágio 1 (core-services): remove links de runtime
// (serviços iniciados sem enable) para que não fiquem supervisionados à toa.
func cmdBootCleanup() int {
	ents, _ := os.ReadDir(filepath.Join(cfg.stateDir, "runtime"))
	for _, e := range ents {
		cleanupRuntime(e.Name())
	}
	os.RemoveAll(cfg.runtimeDir)
	return 0
}

// each executa f para cada unit, acumulando o código de retorno.
func each(args []string, f func(name, kind string) error) int {
	if len(args) == 0 {
		errorf("nenhuma unit informada")
		return 2
	}
	rc := 0
	for _, a := range args {
		name, kind := unitArg(a)
		if err := f(name, kind); err != nil {
			errorf("%v", err)
			rc = 1
		}
	}
	return rc
}

// ======================================================================
// logs estilo journalctl
// ======================================================================
// logSrc é um diretório do svlogd. match != "" filtra linhas do syslog
// (socklog) pelo nome do programa.
type logSrc struct {
	unit, dir, match string
	syslog           bool
}

type logEntry struct {
	t    time.Time
	unit string
	msg  string
	seq  int
}

type logFilter struct {
	since, until time.Time
	grep         *regexp.Regexp
}

func (f logFilter) ok(e logEntry) bool {
	if !f.since.IsZero() && e.t.Before(f.since) {
		return false
	}
	if !f.until.IsZero() && e.t.After(f.until) {
		return false
	}
	if f.grep != nil && !f.grep.MatchString(e.msg) {
		return false
	}
	return true
}

var svlogdRe = regexp.MustCompile(`svlogd\s+([^\n;&|]*)`)

// svlogdDirFromRun descobre o diretório de log a partir do log/run nativo.
func svlogdDirFromRun(name string) string {
	b, err := os.ReadFile(filepath.Join(svDefDir(name), "log", "run"))
	if err != nil {
		return ""
	}
	m := svlogdRe.FindStringSubmatch(string(b))
	if m == nil {
		return ""
	}
	f := strings.Fields(m[1])
	for i := len(f) - 1; i >= 0; i-- {
		if !strings.HasPrefix(f[i], "-") {
			return strings.Trim(f[i], `"'`)
		}
	}
	return ""
}

func socklogEverything() string {
	p := filepath.Join(cfg.socklogDir, "everything")
	if isDir(p) {
		return p
	}
	return ""
}

func unitLogSources(unit string) []logSrc {
	var s []logSrc
	seen := map[string]bool{}
	add := func(d string) {
		if d != "" && isDir(d) && !seen[d] {
			seen[d] = true
			s = append(s, logSrc{unit: unit, dir: d})
		}
	}
	add(filepath.Join(cfg.logDir, unit))
	add(svlogdDirFromRun(unit))
	add(filepath.Join("/var/log", unit)) // alguns serviços do Void usam /var/log/NOME
	if len(s) > 0 {
		// só aceita /var/log/NOME se realmente for um diretório do svlogd
		var out []logSrc
		for _, x := range s {
			if exists(filepath.Join(x.dir, "current")) {
				out = append(out, x)
			}
		}
		s = out
	}
	if len(s) == 0 {
		if d := socklogEverything(); d != "" {
			s = append(s, logSrc{unit: unit, dir: d, match: unit, syslog: true})
		}
	}
	return s
}

func allLogSources() []logSrc {
	var s []logSrc
	ents, _ := os.ReadDir(cfg.logDir)
	for _, e := range ents {
		d := filepath.Join(cfg.logDir, e.Name())
		if exists(filepath.Join(d, "current")) {
			s = append(s, logSrc{unit: e.Name(), dir: d})
		}
	}
	if d := socklogEverything(); d != "" {
		s = append(s, logSrc{dir: d, syslog: true})
	}
	return s
}

// parseStamp reconhece os três formatos do svlogd: -t (tai64n), -tt e -ttt.
func parseStamp(line string) (time.Time, string) {
	if len(line) >= 25 && line[0] == '@' {
		if secs, err := strconv.ParseUint(line[1:17], 16, 64); err == nil {
			nano, _ := strconv.ParseUint(line[17:25], 16, 32)
			msg := strings.TrimPrefix(line[25:], " ")
			if secs > taiOffset {
				return time.Unix(int64(secs-taiOffset), int64(nano)), msg
			}
		}
	}
	if len(line) >= 19 && line[4] == '-' && line[7] == '-' && (line[10] == '_' || line[10] == 'T') && line[13] == ':' {
		sp := strings.IndexByte(line, ' ')
		stamp, msg := line, ""
		if sp > 0 {
			stamp, msg = line[:sp], line[sp+1:]
		}
		stamp = strings.Replace(stamp, "_", "T", 1)
		if t, err := time.ParseInLocation("2006-01-02T15:04:05.999999999", stamp, time.UTC); err == nil {
			return t, msg
		}
	}
	return time.Time{}, line
}

var syslogRe = regexp.MustCompile(`^(?:[a-z0-9]+\.[a-z]+: )?(?:[A-Z][a-z]{2} [ 0-9]\d \d\d:\d\d:\d\d )?([^\s\[:]+)(\[\d+\])?: ?(.*)$`)

// splitSyslog separa "daemon.info: sshd[12]: msg" em (sshd, [12], msg).
func splitSyslog(msg string) (prog, pid, rest string, ok bool) {
	m := syslogRe.FindStringSubmatch(msg)
	if m == nil {
		return "", "", msg, false
	}
	return m[1], m[2], m[3], true
}

func readLogFile(path string, src logSrc, fn func(logEntry)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var last time.Time
	for sc.Scan() {
		if e, ok := makeEntry(sc.Text(), src, &last); ok {
			fn(e)
		}
	}
}

func makeEntry(line string, src logSrc, last *time.Time) (logEntry, bool) {
	t, msg := parseStamp(line)
	if t.IsZero() {
		t = *last
	} else {
		*last = t
	}
	e := logEntry{t: t, unit: src.unit, msg: msg}
	if src.syslog {
		prog, pid, rest, ok := splitSyslog(msg)
		if src.match != "" {
			if !ok || (prog != src.match && filepath.Base(prog) != src.match) {
				return e, false
			}
		}
		if ok {
			if src.match == "" {
				e.unit = prog + pid
			} else {
				e.unit = src.unit + pid
			}
			e.msg = rest
		} else if src.match == "" {
			e.unit = "syslog"
		}
	}
	return e, true
}

func logFiles(dir string) []string {
	old, _ := filepath.Glob(filepath.Join(dir, "@*"))
	sort.Strings(old)
	return append(old, filepath.Join(dir, "current"))
}

func collectLogs(srcs []logSrc, f logFilter) []logEntry {
	var out []logEntry
	seq := 0
	for _, s := range srcs {
		for _, p := range logFiles(s.dir) {
			readLogFile(p, s, func(e logEntry) {
				if f.ok(e) {
					e.seq = seq
					seq++
					out = append(out, e)
				}
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].t.Equal(out[j].t) {
			return out[i].seq < out[j].seq
		}
		return out[i].t.Before(out[j].t)
	})
	return out
}

var (
	errWordRe  = regexp.MustCompile(`(?i)\b(error|erro|fatal|panic|critical|crit|failed|failure|falhou|emerg|alert|segfault)\b`)
	warnWordRe = regexp.MustCompile(`(?i)\b(warn|warning|aviso|deprecated)\b`)
)

func formatEntry(e logEntry, mode, host string) string {
	var ts string
	lt := e.t.Local()
	switch mode {
	case "cat":
		return e.msg
	case "json":
		b, _ := json.Marshal(map[string]string{
			"__REALTIME_TIMESTAMP": strconv.FormatInt(e.t.UnixMicro(), 10),
			"_HOSTNAME":            host,
			"_SYSTEMD_UNIT":        e.unit + ".service",
			"SYSLOG_IDENTIFIER":    e.unit,
			"MESSAGE":              e.msg,
		})
		return string(b)
	case "short-iso":
		ts = lt.Format("2006-01-02T15:04:05-0700")
	case "short-precise":
		ts = lt.Format("Jan 02 15:04:05.000000")
	default:
		ts = lt.Format("Jan 02 15:04:05")
	}
	if e.t.IsZero() {
		ts = strings.Repeat("-", len(ts))
	}
	msg := e.msg
	if red != "" {
		if errWordRe.MatchString(msg) {
			msg = bold + red + msg + reset
		} else if warnWordRe.MatchString(msg) {
			msg = bold + yellow + msg + reset
		}
	}
	return fmt.Sprintf("%s %s %s: %s", ts, host, e.unit, msg)
}

// parseWhen aceita os formatos mais usados do --since/--until do journalctl.
func parseWhen(s string) (time.Time, error) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "now":
		return now, nil
	case "today":
		return today, nil
	case "yesterday":
		return today.AddDate(0, 0, -1), nil
	case "tomorrow":
		return today.AddDate(0, 0, 1), nil
	}
	if strings.HasSuffix(s, " ago") {
		d, err := parseTimespan(strings.TrimSuffix(s, " ago"))
		if err != nil {
			return time.Time{}, err
		}
		return now.Add(-d), nil
	}
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		d, err := parseTimespan(s[1:])
		if err != nil {
			return time.Time{}, err
		}
		if s[0] == '-' {
			d = -d
		}
		return now.Add(d), nil
	}
	for _, l := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t, nil
		}
	}
	for _, l := range []string{"15:04:05", "15:04"} {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return today.Add(time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute + time.Duration(t.Second())*time.Second), nil
		}
	}
	return time.Time{}, fmt.Errorf("data/hora inválida: %q", s)
}

func cmdLogs(args []string) int {
	o, err := parseArgs(args, map[string]fdef{
		"-u": {"unit", true}, "--unit": {"unit", true},
		"-f": {"follow", false}, "--follow": {"follow", false},
		"-n": {"lines", true}, "--lines": {"lines", true},
		"-r": {"reverse", false}, "--reverse": {"reverse", false},
		"-S": {"since", true}, "--since": {"since", true},
		"-U": {"until", true}, "--until": {"until", true},
		"-b": {"boot", false}, "--boot": {"boot", false},
		"-g": {"grep", true}, "--grep": {"grep", true},
		"-o": {"output", true}, "--output": {"output", true},
		"-e": {"end", false}, "--pager-end": {"end", false},
		"-x": {"x", false}, "-a": {"x", false}, "--all": {"x", false},
		"--no-pager": {"nopager", false}, "--no-hostname": {"nohost", false},
		"-D": {"dir", true}, "--directory": {"dir", true},
		"--list": {"list", false}, "-h": {"help", false}, "--help": {"help", false},
		"--case-sensitive": {"case", false},
	})
	if err != nil {
		errorf("%v", err)
		return 2
	}
	if o.bool("help") {
		fmt.Println("uso: voidctl logs [-u NOME]... [-f] [-n N] [-r] [-b] [--since T] [--until T] [-g REGEX] [-o short|short-iso|short-precise|cat|json] [-D DIR] [--list] [--no-pager]")
		return 0
	}
	var srcs []logSrc
	for _, u := range o.strs("unit") {
		name, _ := unitArg(u)
		s := unitLogSources(name)
		if len(s) == 0 {
			warnf("nenhum log encontrado para %s (procurei em %s/%s e no socklog)", name, cfg.logDir, name)
		}
		srcs = append(srcs, s...)
	}
	for _, d := range o.strs("dir") {
		srcs = append(srcs, logSrc{unit: filepath.Base(d), dir: d})
	}
	if len(o.strs("unit")) == 0 && len(o.strs("dir")) == 0 {
		srcs = allLogSources()
	}
	if o.bool("list") {
		for _, s := range srcs {
			fmt.Printf("%-24s %s\n", map[bool]string{true: "(syslog)", false: s.unit}[s.unit == ""], s.dir)
		}
		return 0
	}
	var f logFilter
	if v := o.str("since"); v != "" {
		if f.since, err = parseWhen(v); err != nil {
			errorf("%v", err)
			return 2
		}
	}
	if v := o.str("until"); v != "" {
		if f.until, err = parseWhen(v); err != nil {
			errorf("%v", err)
			return 2
		}
	}
	if o.bool("boot") && f.since.IsZero() {
		f.since = bootTime()
	}
	if g := o.str("grep"); g != "" {
		if !o.bool("case") && strings.ToLower(g) == g {
			g = "(?i)" + g
		}
		if f.grep, err = regexp.Compile(g); err != nil {
			errorf("regex inválida: %v", err)
			return 2
		}
	}
	mode := o.str("output")
	if mode == "" {
		mode = "short"
	}
	host, _ := os.Hostname()
	if o.bool("nohost") {
		host = ""
	}
	follow := o.bool("follow")
	defLines := -1
	if follow {
		defLines = 10
	}
	if o.bool("end") && defLines < 0 {
		defLines = 1000
	}
	n := defLines
	if v := o.str("lines"); v != "" {
		if v == "all" {
			n = -1
		} else if n, err = strconv.Atoi(strings.TrimPrefix(v, "+")); err != nil {
			errorf("-n inválido: %s", v)
			return 2
		}
	}
	entries := collectLogs(srcs, f)
	if n >= 0 && len(entries) > n {
		entries = entries[len(entries)-n:]
	}
	if o.bool("reverse") {
		for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
			entries[i], entries[j] = entries[j], entries[i]
		}
	}
	if mode == "cat" || mode == "json" {
		red, yellow, bold, reset = "", "", "", ""
	}

	var w io.Writer = os.Stdout
	var pager *exec.Cmd
	if !follow && !o.bool("nopager") && isTTY(os.Stdout) && len(entries) > 0 {
		pg := getenv("SYSTEMD_PAGER", getenv("PAGER", "less"))
		if pg != "cat" && pg != "" {
			pager = exec.Command("sh", "-c", pg)
			if strings.HasPrefix(pg, "less") {
				pager.Env = append(os.Environ(), "LESS=FRSXMK")
				if o.bool("end") {
					pager = exec.Command("sh", "-c", pg+" +G")
					pager.Env = append(os.Environ(), "LESS=FRSXMK")
				}
			}
			pager.Stdout, pager.Stderr = os.Stdout, os.Stderr
			if in, err := pager.StdinPipe(); err == nil && pager.Start() == nil {
				w = in
			} else {
				pager = nil
			}
		}
	}
	bw := bufio.NewWriter(w)
	if len(entries) == 0 && !follow && !quiet {
		fmt.Fprintln(os.Stderr, "-- Sem entradas --")
	}
	for _, e := range entries {
		fmt.Fprintln(bw, formatEntry(e, mode, host))
	}
	bw.Flush()
	if pager != nil {
		w.(io.WriteCloser).Close()
		pager.Wait()
	}
	if follow {
		followLogs(srcs, f, mode, host, len(o.strs("unit")) == 0 && len(o.strs("dir")) == 0)
	}
	return 0
}

// ---------- -f ----------

type tailer struct {
	src  logSrc
	path string
	f    *os.File
	ino  uint64
	buf  string
	last time.Time
}

func inode(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}

func (t *tailer) open(atEnd bool) {
	f, err := os.Open(t.path)
	if err != nil {
		return
	}
	fi, _ := f.Stat()
	t.f, t.ino, t.buf = f, inode(fi), ""
	if atEnd {
		f.Seek(0, io.SeekEnd)
	}
}

func (t *tailer) drain(emit func(logEntry)) {
	if t.f == nil {
		return
	}
	data, _ := io.ReadAll(t.f)
	if len(data) == 0 {
		return
	}
	t.buf += string(data)
	for {
		i := strings.IndexByte(t.buf, '\n')
		if i < 0 {
			break
		}
		line := t.buf[:i]
		t.buf = t.buf[i+1:]
		if e, ok := makeEntry(line, t.src, &t.last); ok {
			emit(e)
		}
	}
}

func followLogs(srcs []logSrc, f logFilter, mode, host string, allUnits bool) {
	var ts []*tailer
	known := map[string]bool{}
	add := func(s logSrc, atEnd bool) {
		p := filepath.Join(s.dir, "current")
		key := p + "|" + s.match
		if known[key] {
			return
		}
		known[key] = true
		t := &tailer{src: s, path: p}
		t.open(atEnd)
		ts = append(ts, t)
	}
	for _, s := range srcs {
		add(s, true)
	}
	emit := func(e logEntry) {
		if f.ok(e) {
			fmt.Println(formatEntry(e, mode, host))
		}
	}
	tick := 0
	for {
		time.Sleep(250 * time.Millisecond)
		tick++
		for _, t := range ts {
			t.drain(emit)
			fi, err := os.Stat(t.path)
			if err != nil {
				continue
			}
			if t.f == nil || inode(fi) != t.ino { // svlogd rotacionou
				if t.f != nil {
					t.drain(emit)
					t.f.Close()
				}
				t.open(false)
				t.drain(emit)
			} else if pos, _ := t.f.Seek(0, io.SeekCurrent); fi.Size() < pos {
				t.f.Seek(0, io.SeekStart) // truncado
			}
		}
		if allUnits && tick%20 == 0 { // novos serviços aparecendo
			for _, s := range allLogSources() {
				add(s, false)
			}
		}
	}
}

// ======================================================================
// OnCalendar= e intervalos
// ======================================================================
// ---------- intervalos ("1h 30min", "90s", "2d") ----------

var spanRe = regexp.MustCompile(`^\s*(\d+(?:\.\d+)?)\s*([a-zA-Zµ]*)`)

var spanUnits = map[string]time.Duration{
	"": time.Second, "us": time.Microsecond, "usec": time.Microsecond, "µs": time.Microsecond,
	"ms": time.Millisecond, "msec": time.Millisecond,
	"s": time.Second, "sec": time.Second, "second": time.Second, "seconds": time.Second,
	"m": time.Minute, "min": time.Minute, "minute": time.Minute, "minutes": time.Minute,
	"h": time.Hour, "hr": time.Hour, "hour": time.Hour, "hours": time.Hour,
	"d": 24 * time.Hour, "day": 24 * time.Hour, "days": 24 * time.Hour,
	"w": 7 * 24 * time.Hour, "week": 7 * 24 * time.Hour, "weeks": 7 * 24 * time.Hour,
	"M": 2629800 * time.Second, "month": 2629800 * time.Second, "months": 2629800 * time.Second,
	"y": 31557600 * time.Second, "year": 31557600 * time.Second, "years": 31557600 * time.Second,
}

func parseTimespan(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "infinity" {
		return 1<<63 - 1, nil
	}
	if s == "" {
		return 0, fmt.Errorf("intervalo vazio")
	}
	var total time.Duration
	rest := s
	for strings.TrimSpace(rest) != "" {
		m := spanRe.FindStringSubmatch(rest)
		if m == nil {
			return 0, fmt.Errorf("intervalo inválido: %q", s)
		}
		unit, ok := spanUnits[m[2]]
		if !ok {
			unit, ok = spanUnits[strings.ToLower(m[2])]
		}
		if !ok {
			return 0, fmt.Errorf("unidade desconhecida %q em %q", m[2], s)
		}
		v, _ := strconv.ParseFloat(m[1], 64)
		total += time.Duration(v * float64(unit))
		rest = rest[len(m[0]):]
	}
	return total, nil
}

// ---------- expressões OnCalendar ----------

type field struct {
	any  bool
	set  map[int]bool
	name string
}

func (f field) match(v int) bool { return f.any || f.set[v] }

type calSpec struct {
	raw                                    string
	wday                                   field
	year, month, day, hour, minute, second field
	lastDay                                bool // "~" — dias contados a partir do fim do mês
	loc                                    *time.Location
}

var calShorthands = map[string]string{
	"minutely":     "*-*-* *:*:00",
	"hourly":       "*-*-* *:00:00",
	"daily":        "*-*-* 00:00:00",
	"monthly":      "*-*-01 00:00:00",
	"weekly":       "Mon *-*-* 00:00:00",
	"yearly":       "*-01-01 00:00:00",
	"annually":     "*-01-01 00:00:00",
	"quarterly":    "*-01,04,07,10-01 00:00:00",
	"semiannually": "*-01,07-01 00:00:00",
}

var wdayNames = map[string]int{
	"sun": 0, "sunday": 0, "mon": 1, "monday": 1, "tue": 2, "tuesday": 2,
	"wed": 3, "wednesday": 3, "thu": 4, "thursday": 4, "fri": 5, "friday": 5,
	"sat": 6, "saturday": 6,
}

func parseField(s string, min, max int, name string) (field, error) {
	f := field{name: name, set: map[int]bool{}}
	if s == "*" {
		f.any = true
		return f, nil
	}
	for _, part := range strings.Split(s, ",") {
		step := 1
		if i := strings.Index(part, "/"); i >= 0 {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n <= 0 {
				return f, fmt.Errorf("passo inválido em %s: %q", name, part)
			}
			step, part = n, part[:i]
			if part == "*" {
				part = strconv.Itoa(min)
			}
			if !strings.Contains(part, "..") {
				part += ".." + strconv.Itoa(max)
			}
		}
		lo, hi := part, part
		if i := strings.Index(part, ".."); i >= 0 {
			lo, hi = part[:i], part[i+2:]
		}
		a, err1 := strconv.Atoi(strings.SplitN(lo, ".", 2)[0])
		b, err2 := strconv.Atoi(strings.SplitN(hi, ".", 2)[0])
		if err1 != nil || err2 != nil || a < min || b > max || a > b {
			return f, fmt.Errorf("valor inválido para %s: %q (faixa %d..%d)", name, part, min, max)
		}
		for v := a; v <= b; v += step {
			f.set[v] = true
		}
	}
	return f, nil
}

func parseWeekdays(s string) (field, error) {
	f := field{name: "dia da semana", set: map[int]bool{}}
	for _, part := range strings.Split(s, ",") {
		lo, hi := part, part
		if i := strings.Index(part, ".."); i >= 0 {
			lo, hi = part[:i], part[i+2:]
		} else if i := strings.Index(part, "-"); i >= 0 {
			lo, hi = part[:i], part[i+1:]
		}
		a, ok1 := wdayNames[strings.ToLower(lo)]
		b, ok2 := wdayNames[strings.ToLower(hi)]
		if !ok1 || !ok2 {
			return f, fmt.Errorf("dia da semana inválido: %q", part)
		}
		for v := a; ; v = (v + 1) % 7 {
			f.set[v] = true
			if v == b {
				break
			}
		}
	}
	return f, nil
}

func parseCalendar(expr string) (*calSpec, error) {
	c := &calSpec{raw: expr, loc: time.Local}
	e := strings.TrimSpace(expr)
	toks := strings.Fields(e)
	// fuso horário no final (ex: "UTC", "America/Sao_Paulo")
	if n := len(toks); n > 1 || (n == 1 && calShorthands[strings.ToLower(toks[0])] == "") {
		last := toks[n-1]
		looksDate := strings.ContainsAny(last, ":*0123456789")
		_, isShort := calShorthands[strings.ToLower(last)]
		_, wErr := parseWeekdays(last)
		if !looksDate && !isShort && wErr != nil {
			if loc, err := time.LoadLocation(last); err == nil {
				c.loc = loc
				toks = toks[:n-1]
			}
		}
	}
	if len(toks) == 1 {
		if s, ok := calShorthands[strings.ToLower(toks[0])]; ok {
			toks = strings.Fields(s)
		}
	}
	c.wday = field{any: true}
	var datePart, timePart string
	for _, t := range toks {
		switch {
		case strings.Contains(t, ":"):
			if timePart != "" {
				return nil, fmt.Errorf("mais de uma hora em %q", expr)
			}
			timePart = t
		case strings.Contains(t, "-") && (strings.ContainsAny(t, "0123456789*")):
			if datePart != "" {
				return nil, fmt.Errorf("mais de uma data em %q", expr)
			}
			datePart = t
		default:
			w, err := parseWeekdays(t)
			if err != nil {
				return nil, err
			}
			c.wday = w
		}
	}
	if datePart == "" {
		datePart = "*-*-*"
	}
	if timePart == "" {
		timePart = "00:00:00"
	}
	if strings.Contains(datePart, "~") {
		c.lastDay = true
		datePart = strings.Replace(datePart, "~", "-", 1)
	}
	dp := strings.Split(datePart, "-")
	if len(dp) == 2 {
		dp = append([]string{"*"}, dp...)
	}
	if len(dp) != 3 {
		return nil, fmt.Errorf("data inválida: %q", datePart)
	}
	var err error
	if c.year, err = parseField(dp[0], 1970, 2199, "ano"); err != nil {
		return nil, err
	}
	if c.month, err = parseField(dp[1], 1, 12, "mês"); err != nil {
		return nil, err
	}
	if c.day, err = parseField(dp[2], 1, 31, "dia"); err != nil {
		return nil, err
	}
	tp := strings.Split(timePart, ":")
	if len(tp) == 2 {
		tp = append(tp, "00")
	}
	if len(tp) != 3 {
		return nil, fmt.Errorf("hora inválida: %q", timePart)
	}
	if c.hour, err = parseField(tp[0], 0, 23, "hora"); err != nil {
		return nil, err
	}
	if c.minute, err = parseField(tp[1], 0, 59, "minuto"); err != nil {
		return nil, err
	}
	if c.second, err = parseField(tp[2], 0, 59, "segundo"); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *calSpec) matchDay(d time.Time) bool {
	if !c.year.match(d.Year()) || !c.month.match(int(d.Month())) || !c.wday.match(int(d.Weekday())) {
		return false
	}
	day := d.Day()
	if c.lastDay {
		last := time.Date(d.Year(), d.Month()+1, 0, 0, 0, 0, 0, d.Location()).Day()
		day = last - d.Day() + 1 // ~01 = último dia
	}
	return c.day.match(day)
}

// next devolve o primeiro instante estritamente depois de after.
func (c *calSpec) next(after time.Time) (time.Time, bool) {
	t := after.In(c.loc).Truncate(time.Second).Add(time.Second)
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, c.loc)
	for i := 0; i < 366*230; i++ {
		d := day.AddDate(0, 0, i)
		if d.Year() > 2199 {
			break
		}
		if !c.matchDay(d) {
			continue
		}
		same := i == 0
		for h := 0; h < 24; h++ {
			if !c.hour.match(h) || (same && h < t.Hour()) {
				continue
			}
			for m := 0; m < 60; m++ {
				if !c.minute.match(m) || (same && h == t.Hour() && m < t.Minute()) {
					continue
				}
				for s := 0; s < 60; s++ {
					if !c.second.match(s) || (same && h == t.Hour() && m == t.Minute() && s < t.Second()) {
						continue
					}
					cand := time.Date(d.Year(), d.Month(), d.Day(), h, m, s, 0, c.loc)
					if cand.Hour() != h || cand.Before(t) { // horário inexistente (horário de verão)
						continue
					}
					return cand, true
				}
			}
		}
	}
	return time.Time{}, false
}

// normalized imprime a forma canônica, como o systemd-analyze calendar.
func (c *calSpec) normalized() string {
	f := func(fl field, w int) string {
		if fl.any {
			return "*"
		}
		var vals []int
		for v := range fl.set {
			vals = append(vals, v)
		}
		sortInts(vals)
		var parts []string
		for i := 0; i < len(vals); {
			j := i
			for j+1 < len(vals) && vals[j+1] == vals[j]+1 {
				j++
			}
			if j-i >= 2 {
				parts = append(parts, fmt.Sprintf("%0*d..%0*d", w, vals[i], w, vals[j]))
			} else {
				for k := i; k <= j; k++ {
					parts = append(parts, fmt.Sprintf("%0*d", w, vals[k]))
				}
			}
			i = j + 1
		}
		return strings.Join(parts, ",")
	}
	s := ""
	if !c.wday.any {
		names := []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
		var p []string
		for _, i := range []int{1, 2, 3, 4, 5, 6, 0} {
			if c.wday.set[i] {
				p = append(p, names[i])
			}
		}
		s = strings.Join(p, ",") + " "
	}
	sep := "-"
	if c.lastDay {
		sep = "~"
	}
	s += f(c.year, 4) + "-" + f(c.month, 2) + sep + f(c.day, 2) + " " + f(c.hour, 2) + ":" + f(c.minute, 2) + ":" + f(c.second, 2)
	if c.loc != time.Local {
		s += " " + c.loc.String()
	}
	return s
}

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func cmdCalendar(args []string) int {
	o, err := parseArgs(args, map[string]fdef{"--iterations": {"it", true}})
	if err != nil || len(o.pos) == 0 {
		errorf("uso: voidctl calendar [--iterations=N] EXPR...")
		return 2
	}
	it, _ := o.int("it", 1)
	rc := 0
	for i, e := range o.pos {
		if i > 0 {
			fmt.Println()
		}
		c, err := parseCalendar(e)
		if err != nil {
			errorf("%s: %v", e, err)
			rc = 1
			continue
		}
		fmt.Printf("  Original form: %s\n", e)
		fmt.Printf("Normalized form: %s\n", c.normalized())
		t := time.Now()
		for k := 0; k < it; k++ {
			n, ok := c.next(t)
			if !ok {
				fmt.Println("    Next elapse: never")
				break
			}
			label := "    Next elapse"
			if k > 0 {
				label = fmt.Sprintf("       Iter. #%d", k+1)
			}
			fmt.Printf("%s: %s\n", label, fmtStamp(n))
			if k == 0 {
				fmt.Printf("       (in UTC): %s\n", n.UTC().Format("Mon 2006-01-02 15:04:05 MST"))
				fmt.Printf("       From now: %s left\n", fmtSpan(time.Until(n)))
			}
			t = n
		}
	}
	return rc
}

func cmdTimespan(args []string) int {
	rc := 0
	for _, a := range args {
		d, err := parseTimespan(a)
		if err != nil {
			errorf("%v", err)
			rc = 1
			continue
		}
		fmt.Printf("Original: %s\n      μs: %d\n   Human: %s\n", a, d.Microseconds(), fmtSpan(d))
	}
	return rc
}

// ======================================================================
// timers e daemon voidctl-timerd
// ======================================================================
const timerdName = "voidctl-timerd"

type timerDef struct {
	name, unit, desc, path string
	cals                   []*calSpec
	onBoot, onStartup      []time.Duration
	onActive               []time.Duration
	onUnitActive           []time.Duration
	persistent             bool
	randomDelay            time.Duration
	enabled                bool
}

func timerEnabledLink(name string) string {
	return filepath.Join(cfg.timerDir, "enabled", name+".timer")
}

func loadTimerDef(path string) (*timerDef, error) {
	uname := filepath.Base(path)
	u := newUnit(uname)
	u.path = path
	if err := u.load(path); err != nil {
		return nil, err
	}
	return timerFromUnit(u)
}

func timerFromUnit(u *unitFile) (*timerDef, error) {
	t := &timerDef{name: u.base(), path: u.path, desc: u.get("Unit", "Description")}
	t.unit = strings.TrimSuffix(u.get("Timer", "Unit"), ".service")
	if t.unit == "" {
		t.unit = u.base()
	}
	for _, e := range u.all("Timer", "OnCalendar") {
		c, err := parseCalendar(e)
		if err != nil {
			return nil, fmt.Errorf("%s: OnCalendar=%s: %v", u.name, e, err)
		}
		t.cals = append(t.cals, c)
	}
	spans := func(key string) ([]time.Duration, error) {
		var out []time.Duration
		for _, v := range u.all("Timer", key) {
			d, err := parseTimespan(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %s=%s: %v", u.name, key, v, err)
			}
			out = append(out, d)
		}
		return out, nil
	}
	var err error
	if t.onBoot, err = spans("OnBootSec"); err != nil {
		return nil, err
	}
	if t.onStartup, err = spans("OnStartupSec"); err != nil {
		return nil, err
	}
	if t.onActive, err = spans("OnActiveSec"); err != nil {
		return nil, err
	}
	if t.onUnitActive, err = spans("OnUnitActiveSec"); err != nil {
		return nil, err
	}
	inact, err := spans("OnUnitInactiveSec")
	if err != nil {
		return nil, err
	}
	t.onUnitActive = append(t.onUnitActive, inact...) // aproximação
	t.persistent = parseBool(u.get("Timer", "Persistent"))
	if v := u.get("Timer", "RandomizedDelaySec"); v != "" {
		t.randomDelay, _ = parseTimespan(v)
	}
	if len(t.cals)+len(t.onBoot)+len(t.onStartup)+len(t.onActive)+len(t.onUnitActive) == 0 {
		return nil, fmt.Errorf("%s: nenhum gatilho (OnCalendar=, OnBootSec=, OnUnitActiveSec=...)", u.name)
	}
	t.enabled = lexists(timerEnabledLink(t.name))
	return t, nil
}

func loadAllTimers() []*timerDef {
	paths, _ := filepath.Glob(filepath.Join(cfg.timerDir, "*.timer"))
	sort.Strings(paths)
	var out []*timerDef
	for _, p := range paths {
		t, err := loadTimerDef(p)
		if err != nil {
			warnf("%v", err)
			continue
		}
		out = append(out, t)
	}
	return out
}

func stampPath(name string) string { return filepath.Join(cfg.stateDir, "timers", name) }

func lastTrigger(name string) time.Time {
	b, err := os.ReadFile(stampPath(name))
	if err != nil {
		return time.Time{}
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return time.Unix(n, 0)
}

func timerdStart() time.Time {
	b, err := os.ReadFile(filepath.Join(cfg.runtimeDir, "timerd.start"))
	if err != nil {
		return time.Now()
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return time.Unix(n, 0)
}

// nextElapse calcula o próximo disparo. Um instante no passado significa
// "atrasado — dispare agora".
func (t *timerDef) nextElapse(now, boot, active, last time.Time) (time.Time, bool) {
	var best time.Time
	consider := func(c time.Time) {
		if best.IsZero() || c.Before(best) {
			best = c
		}
	}
	for _, c := range t.cals {
		ref := now
		if t.persistent && !last.IsZero() && last.Before(now) {
			ref = last // perdeu um disparo com a máquina desligada? dispara já
		}
		if n, ok := c.next(ref); ok {
			consider(n)
		}
	}
	for _, d := range append(append([]time.Duration{}, t.onBoot...), t.onStartup...) {
		if at := boot.Add(d); last.Before(at) {
			consider(at)
		}
	}
	for _, d := range t.onActive {
		if at := active.Add(d); last.Before(at) {
			consider(at)
		}
	}
	for _, d := range t.onUnitActive {
		if !last.IsZero() {
			consider(last.Add(d))
		}
	}
	return best, !best.IsZero()
}

// ---------- import / enable / disable ----------

func importTimer(path, uname string, io importOpts) (string, error) {
	u, err := loadUnit(path, uname)
	if err != nil {
		return "", err
	}
	t, err := timerFromUnit(u)
	if err != nil {
		return "", err
	}
	// serviço ativado pelo timer
	if !serviceExists(t.unit) || readMeta(t.unit) != nil {
		spath, sname, err := findUnit(t.unit + ".service")
		if err != nil {
			if !serviceExists(t.unit) {
				return "", fmt.Errorf("%s ativa %s.service, que não foi encontrado: %v", uname, t.unit, err)
			}
		} else if _, err := importService(spath, sname, importOpts{name: t.unit, timer: t.name, force: io.force, dryRun: io.dryRun}); err != nil {
			return "", err
		}
	} else {
		warnf("%s ativa o serviço nativo %s; ele será disparado com 'sv up'", uname, t.unit)
	}
	dst := filepath.Join(cfg.timerDir, t.name+".timer")
	content := u.raw.String()
	if io.dryRun {
		fmt.Printf("%s# %s%s\n%s\n", cyan, dst, reset, content)
		return t.name, nil
	}
	if err := os.MkdirAll(cfg.timerDir, 0755); err != nil {
		return "", err
	}
	if err := atomicWrite(dst, []byte(content), 0644); err != nil {
		return "", err
	}
	notef("%s → %s", path, dst)
	if readStatus(timerdName).supervised {
		svControl(timerdName, "h")
	}
	return t.name, nil
}

func enableTimer(name string) error {
	p := filepath.Join(cfg.timerDir, name+".timer")
	t, err := loadTimerDef(p)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("timer %s.timer não importado (use: voidctl import %s.timer)", name, name)
		}
		return err
	}
	if !serviceExists(t.unit) {
		return fmt.Errorf("%s.timer ativa %s, que não existe em %s", name, t.unit, cfg.svDir)
	}
	os.MkdirAll(filepath.Dir(timerEnabledLink(name)), 0755)
	if !lexists(timerEnabledLink(name)) {
		if err := os.Symlink("../"+name+".timer", timerEnabledLink(name)); err != nil {
			return err
		}
		notef("Created symlink %s → %s.", timerEnabledLink(name), p)
	}
	// o serviço precisa estar supervisionado (com down) para o timerd dispará-lo
	if !isLinked(t.unit) {
		downf := filepath.Join(svDefDir(t.unit), "down")
		if !exists(downf) {
			os.WriteFile(downf, nil, 0644)
		}
		if err := os.Symlink(svDefDir(t.unit), svLink(t.unit)); err != nil {
			return err
		}
	}
	if err := ensureTimerd(); err != nil {
		return err
	}
	svControl(timerdName, "h")
	return nil
}

func disableTimer(name string) error {
	if lexists(timerEnabledLink(name)) {
		os.Remove(timerEnabledLink(name))
		notef("Removed %s.", timerEnabledLink(name))
	}
	if t, err := loadTimerDef(filepath.Join(cfg.timerDir, name+".timer")); err == nil {
		if m := readMeta(t.unit); m != nil && m["timer"] == name && isLinked(t.unit) && readStatus(t.unit).state == stDown {
			os.Remove(svLink(t.unit))
		}
	}
	if readStatus(timerdName).supervised {
		svControl(timerdName, "h")
	}
	return nil
}

// ensureTimerd cria e habilita o serviço runit do daemon de timers.
func ensureTimerd() error {
	dir := svDefDir(timerdName)
	if !exists(filepath.Join(dir, "run")) {
		self, err := os.Executable()
		if err != nil {
			self = "/usr/bin/voidctl"
		}
		if err := os.MkdirAll(filepath.Join(dir, "log"), 0755); err != nil {
			return err
		}
		envs := ""
		for _, k := range []string{"VOIDCTL_SVDIR", "VOIDCTL_RUNDIR", "VOIDCTL_LOGDIR", "VOIDCTL_CONFDIR", "VOIDCTL_TIMERDIR", "VOIDCTL_STATEDIR", "VOIDCTL_RUNTIMEDIR"} {
			if v := os.Getenv(k); v != "" {
				envs += fmt.Sprintf("export %s=%s\n", k, shq(v))
			}
		}
		run := fmt.Sprintf("#!/bin/sh\n# daemon de timers do voidctl (equivalente aos .timer do systemd)\nexec 2>&1\n%sexec %s timerd\n", envs, shq(self))
		if err := os.WriteFile(filepath.Join(dir, "run"), []byte(run), 0755); err != nil {
			return err
		}
		logd := filepath.Join(cfg.logDir, timerdName)
		os.WriteFile(filepath.Join(dir, "log", "run"), []byte(fmt.Sprintf("#!/bin/sh\nmkdir -p %s\nexec svlogd -tt %s\n", shq(logd), shq(logd))), 0755)
		notef("Criado o serviço %s (daemon de timers).", dir)
	}
	if !isLinked(timerdName) || isRuntime(timerdName) {
		return enableUnit(timerdName, true)
	}
	if readStatus(timerdName).state != stRun {
		return startUnit(timerdName, map[string]bool{})
	}
	return nil
}

// ---------- list-timers / status ----------

func cmdListTimers(args []string) int {
	o, _ := parseArgs(args, map[string]fdef{"--all": {"all", false}, "-a": {"all", false}, "--no-pager": {"np", false}})
	all := o != nil && o.bool("all")
	now := time.Now()
	boot, act := bootTime(), timerdStart()
	type row struct {
		next time.Time
		cols []string
	}
	var rows []row
	for _, t := range loadAllTimers() {
		if !t.enabled && !all {
			continue
		}
		last := lastTrigger(t.name)
		next, ok := t.nextElapse(now, boot, act, last)
		nextS, left := "-", "-"
		if ok && t.enabled {
			if next.Before(now) {
				next = now
			}
			nextS, left = fmtStamp(next), fmtSpan(next.Sub(now))
		} else {
			next = time.Unix(1<<40, 0)
		}
		lastS, passed := "-", "-"
		if !last.IsZero() {
			lastS, passed = fmtStamp(last), fmtSpan(now.Sub(last))+" ago"
		}
		name := t.name + ".timer"
		if !t.enabled {
			name = gray + name + " (disabled)" + reset
		}
		rows = append(rows, row{next, []string{nextS, left, lastS, passed, name, t.unit + ".service"}})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].next.Before(rows[j].next) })
	var r [][]string
	for _, x := range rows {
		r = append(r, x.cols)
	}
	printTable([]string{"NEXT", "LEFT", "LAST", "PASSED", "UNIT", "ACTIVATES"}, r)
	fmt.Printf("\n%d timers listados.", len(r))
	if !all {
		fmt.Print(" Use --all para ver também os desabilitados.")
	}
	fmt.Println()
	if len(r) > 0 && !readStatus(timerdName).supervised {
		warnf("o daemon %s não está rodando; os timers não vão disparar (voidctl enable --now %s)", timerdName, timerdName)
	}
	return 0
}

func statusTimer(name string) error {
	t, err := loadTimerDef(filepath.Join(cfg.timerDir, name+".timer"))
	if err != nil {
		errorf("Unit %s.timer could not be found.", name)
		return err
	}
	now := time.Now()
	last := lastTrigger(name)
	dot, act := "○", "inactive (dead)"
	if t.enabled {
		dot, act = green+"●"+reset, bold+green+"active"+reset+" (waiting)"
		if !readStatus(timerdName).supervised {
			dot, act = yellow+"●"+reset, bold+yellow+"active"+reset+" (timerd parado!)"
		}
	}
	desc := t.desc
	if desc == "" {
		desc = name + ".timer"
	}
	en := "disabled"
	if t.enabled {
		en = "enabled"
	}
	fmt.Printf("%s %s%s.timer%s - %s\n", dot, bold, name, reset, desc)
	fmt.Printf("     Loaded: loaded (%s; %s)\n", t.path, en)
	fmt.Printf("     Active: %s\n", act)
	if next, ok := t.nextElapse(now, bootTime(), timerdStart(), last); ok && t.enabled {
		if next.Before(now) {
			next = now
		}
		fmt.Printf("    Trigger: %s; %s left\n", fmtStamp(next), fmtSpan(next.Sub(now)))
	}
	if !last.IsZero() {
		fmt.Printf("  Triggered: %s; %s ago\n", fmtStamp(last), fmtSpan(now.Sub(last)))
	}
	for _, c := range t.cals {
		fmt.Printf("   Calendar: %s\n", c.normalized())
	}
	fmt.Printf("   Triggers: ● %s.service\n", t.unit)
	return nil
}

// ---------- daemon ----------

func logd(format string, a ...any) {
	fmt.Printf(format+"\n", a...)
}

func cmdTimerd(args []string) int {
	start := time.Now()
	os.MkdirAll(cfg.runtimeDir, 0755)
	os.MkdirAll(filepath.Dir(stampPath("x")), 0755)
	os.WriteFile(filepath.Join(cfg.runtimeDir, "timerd.start"), []byte(strconv.FormatInt(start.Unix(), 10)), 0644)
	boot := bootTime()

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM, syscall.SIGINT)

	jitter := map[string]time.Duration{}
	load := func() []*timerDef {
		var out []*timerDef
		for _, t := range loadAllTimers() {
			if t.enabled {
				out = append(out, t)
			}
		}
		logd("%d timer(s) habilitado(s)", len(out))
		return out
	}
	timers := load()
	for {
		now := time.Now()
		wake := now.Add(time.Minute)
		for _, t := range timers {
			last := lastTrigger(t.name)
			next, ok := t.nextElapse(now, boot, start, last)
			if !ok {
				continue
			}
			if t.randomDelay > 0 {
				if _, has := jitter[t.name]; !has {
					jitter[t.name] = time.Duration(rand.Int63n(int64(t.randomDelay)))
				}
				next = next.Add(jitter[t.name])
			}
			if !next.After(now) {
				fire(t)
				delete(jitter, t.name)
				wake = now.Add(time.Second) // reavalia logo o próximo disparo
				continue
			}
			if next.Before(wake) {
				wake = next
			}
		}
		select {
		case <-hup:
			logd("recarregando timers")
			timers = load()
		case <-term:
			logd("encerrando")
			return 0
		case <-time.After(time.Until(wake)):
		}
	}
}

func fire(t *timerDef) {
	os.WriteFile(stampPath(t.name), []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0644)
	st := readStatus(t.unit)
	if !st.supervised {
		logd("%s.timer: %s não está supervisionado (link em %s ausente?)", t.name, t.unit, cfg.runDir)
		return
	}
	if st.state == stRun {
		logd("%s.timer: %s já está rodando, disparo ignorado", t.name, t.unit)
		return
	}
	logd("%s.timer: iniciando %s.service", t.name, t.unit)
	if err := svControl(t.unit, "u"); err != nil {
		logd("%s.timer: %v", t.name, err)
	}
}
