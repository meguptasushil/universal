package portal

import (
	"context"
	"io"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ScanPorts are the ports checked for apps already running on this computer.
var ScanPorts = []int{80, 1313, 3000, 3001, 4000, 4200, 4321, 5000, 5173, 5500, 7860, 8000, 8001, 8080, 8081, 8501, 8888, 9000,
	22, 1883, 3306, 5432, 6379, 25565, 27017}

var knownTCP = map[int]string{
	22: "SSH", 1883: "MQTT broker", 3306: "MySQL / MariaDB", 5432: "PostgreSQL",
	6379: "Redis", 25565: "Minecraft server", 27017: "MongoDB",
}

// Found is something listening on this computer.
type Found struct {
	Port      int    `json:"port"`
	Kind      string `json:"kind"` // web or tcp
	Name      string `json:"name"`
	Published bool   `json:"published"`
}

var titleRE = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// probe reports whether a port is open, and if it speaks HTTP, its page title.
func probe(ctx context.Context, port int) (open, web bool, title string) {
	addr := "127.0.0.1:" + strconv.Itoa(port)
	c, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
	if err != nil {
		return false, false, ""
	}
	c.Close()
	if _, known := knownTCP[port]; known {
		return true, false, ""
	}
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+addr+"/", nil)
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		return true, false, ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if m := titleRE.FindSubmatch(b); m != nil {
		title = strings.Join(strings.Fields(string(m[1])), " ")
		if len(title) > 60 {
			title = title[:60]
		}
	}
	return true, true, title
}

// Discover lists apps listening on the common ports, skipping the given ports
// (the portal's own).
func (m *Manager) Discover(ctx context.Context, skip ...int) []Found {
	skipped := map[int]bool{}
	for _, p := range skip {
		skipped[p] = true
	}
	published := map[int]bool{}
	for _, a := range m.State().Apps {
		if _, p, err := net.SplitHostPort(a.LocalAddr); err == nil && (a.Kind == "web" || a.Kind == "tcp") {
			n, _ := strconv.Atoi(p)
			published[n] = true
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var out []Found
	for _, port := range ScanPorts {
		if skipped[port] {
			continue
		}
		wg.Add(1)
		go func(port int) {
			defer wg.Done()
			open, web, title := probe(ctx, port)
			if !open {
				return
			}
			f := Found{Port: port, Kind: "tcp", Published: published[port]}
			switch {
			case web && title != "":
				f.Kind, f.Name = "web", title
			case web:
				f.Kind, f.Name = "web", "Web app on port "+strconv.Itoa(port)
			case knownTCP[port] != "":
				f.Name = knownTCP[port]
			default:
				f.Name = "Service on port " + strconv.Itoa(port)
			}
			mu.Lock()
			out = append(out, f)
			mu.Unlock()
		}(port)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == "web"
		}
		return out[i].Port < out[j].Port
	})
	return out
}
