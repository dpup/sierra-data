// Command test-meshcore is the CLI diagnostic for the MeshCore MQTT bridge
// source (peer of test-google/test-caltrans/test-weather). It connects to a
// bridge with a subscriber credential, subscribes, tallies packet types,
// optionally captures every message to a JSONL file, and (verbose) decodes
// ADVERTs via meshcore.DecodeFrame to sanity-check the framing against live
// traffic — useful for validating broker access and the decoder before enabling
// the source in prefab.yaml.
//
// Configured entirely by env so credentials never land in argv or on disk:
//
//	MC_URLS    comma-separated candidate broker URLs (first that connects wins)
//	MC_HOST    host for default candidate URLs when MC_URLS unset
//	MC_USER    subscriber username
//	MC_PASS    subscriber password
//	MC_CLIENTID MQTT client id (default sierra-test-meshcore)
//	MC_TOPICS  comma-separated subscribe topics (default: meshcore/#)
//	MC_SECS    capture window seconds (default: 45)
//	MC_OUT     if set, append every message as JSONL {t,topic,payload} here
//	MC_VERBOSE if set, print per-advert decode results
//
// Example:
//
//	MC_HOST=mqtt.gomesh.dev MC_USER=<u> MC_PASS=<p> MC_VERBOSE=1 ./bin/test-meshcore
package main

import (
	"bufio"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/dpup/sierra-data/internal/clients/meshcore"
)

type envelope struct {
	PacketType json.RawMessage `json:"packet_type"`
	Raw        string          `json:"raw"`
}

func main() {
	urls := splitEnv("MC_URLS")
	if len(urls) == 0 {
		h := envOr("MC_HOST", "mqtt.gomesh.dev")
		urls = []string{
			"wss://" + h + ":443/mqtt",
			"wss://" + h + "/mqtt",
			"wss://" + h + ":443/ws",
			"wss://" + h + ":443",
		}
	}
	topics := splitEnv("MC_TOPICS")
	if len(topics) == 0 {
		topics = []string{"meshcore/#"}
	}
	secs := atoiOr(os.Getenv("MC_SECS"), 45)
	verbose := os.Getenv("MC_VERBOSE") != ""

	var out *bufio.Writer
	if p := os.Getenv("MC_OUT"); p != "" {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Printf("cannot open MC_OUT %s: %v\n", p, err)
			os.Exit(1)
		}
		defer f.Close()
		out = bufio.NewWriter(f)
		defer out.Flush()
		fmt.Printf("capturing JSONL → %s\n", p)
	}

	var (
		mu       sync.Mutex
		total    int
		byType   = map[int64]int{}
		adverts  int
		okDecode int
	)

	handler := func(_ mqtt.Client, m mqtt.Message) {
		mu.Lock()
		defer mu.Unlock()
		total++
		if out != nil {
			line, _ := json.Marshal(map[string]any{
				"t":       time.Now().UTC().Format(time.RFC3339Nano),
				"topic":   m.Topic(),
				"payload": json.RawMessage(m.Payload()),
			})
			out.Write(line)
			out.WriteByte('\n')
		}
		var env envelope
		if err := json.Unmarshal(m.Payload(), &env); err != nil {
			return
		}
		pt := numInt(env.PacketType)
		byType[pt]++
		if pt != 4 {
			return
		}
		adverts++
		if !verbose {
			return
		}
		raw, err := hex.DecodeString(strings.TrimSpace(env.Raw))
		if err != nil {
			return
		}
		adv, err := meshcore.DecodeFrame(raw)
		if err != nil {
			fmt.Printf("  DECODE FAIL rawlen=%d hdr=%02x b1=%02x: %v\n", len(raw), raw[0], raw[1], err)
			return
		}
		loc := "no-loc"
		if adv.HasLocation {
			loc = fmt.Sprintf("%.5f,%.5f", adv.Lat, adv.Lng)
		}
		if adv.SignatureValid {
			okDecode++
		}
		fmt.Printf("  ADVERT pubkey=%s… role=%-11s sig=%-5v %s name=%q\n", adv.PubKey[:12], adv.Role, adv.SignatureValid, loc, adv.Name)
	}

	var client mqtt.Client
	var connectedURL string
	for _, u := range urls {
		opts := mqtt.NewClientOptions().
			AddBroker(u).SetClientID(envOr("MC_CLIENTID", "sierra-test-meshcore")).
			SetConnectTimeout(12 * time.Second).SetKeepAlive(30 * time.Second).SetCleanSession(true)
		if v := os.Getenv("MC_USER"); v != "" {
			opts.SetUsername(v)
		}
		if v := os.Getenv("MC_PASS"); v != "" {
			opts.SetPassword(v)
		}
		if strings.HasPrefix(strings.ToLower(u), "wss://") || strings.HasPrefix(strings.ToLower(u), "ssl://") {
			opts.SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12})
		}
		c := mqtt.NewClient(opts)
		fmt.Printf("connecting → %s ... ", u)
		tok := c.Connect()
		if tok.WaitTimeout(14*time.Second) && tok.Error() == nil {
			fmt.Println("OK")
			client, connectedURL = c, u
			break
		}
		fmt.Printf("FAILED: %v\n", tok.Error())
	}
	if client == nil {
		fmt.Println("RESULT: could not connect to any candidate URL.")
		os.Exit(1)
	}
	for _, t := range topics {
		if tok := client.Subscribe(t, 0, handler); tok.WaitTimeout(8*time.Second) && tok.Error() != nil {
			fmt.Printf("SUBSCRIBE FAILED %s: %v\n", t, tok.Error())
		} else {
			fmt.Printf("subscribed → %s\n", t)
		}
	}

	fmt.Printf("capturing %ds on %s ...\n", secs, connectedURL)
	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(60 * time.Second)
		mu.Lock()
		if out != nil {
			out.Flush()
		}
		fmt.Printf("[%s] msgs=%d types=%s adverts=%d\n", time.Now().UTC().Format("15:04:05"), total, typeHist(byType), adverts)
		mu.Unlock()
	}
	client.Disconnect(250)

	mu.Lock()
	defer mu.Unlock()
	if out != nil {
		out.Flush()
	}
	fmt.Printf("\n===== SUMMARY =====\nmessages=%d\ntypes=%s\nadverts(type4)=%d  clean-as-is-decodes=%d\n", total, typeHist(byType), adverts, okDecode)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func atoiOr(s string, d int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return d
}
func splitEnv(k string) []string {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
func numInt(r json.RawMessage) int64 {
	s := strings.Trim(strings.TrimSpace(string(r)), `"`)
	if s == "" || s == "null" {
		return -1
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int64(f)
	}
	return -1
}
func typeHist(m map[int64]int) string {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, int(k))
	}
	sort.Ints(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "t%d=%d", k, m[int64(k)])
	}
	if b.Len() == 0 {
		return "(none)"
	}
	return b.String()
}
