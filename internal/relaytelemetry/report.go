package relaytelemetry

import (
	"sort"
	"time"
)

// Sample is one reading of a node. Nil fields were not answered: they are
// sent as JSON null, never zero.
type Sample struct {
	BatteryVoltage *float64
	UptimeS        *int64
	TxQueueLen     *int32
	NoiseFloorDBm  *int32
	LastRSSIdBm    *int32
	LastSNRdB      *float64
	AirtimeMs      *int64
	RxAirtimeMs    *int64
	NbSent         *int64
	NbRecv         *int64
	SentFlood      *int64
	SentDirect     *int64
	RecvFlood      *int64
	RecvDirect     *int64
	RecvErrors     *int64
}

// Report is the mesh.repeater payload (docs/mesh-reporter-guide.md), the same
// document alanpi sends.
type Report struct {
	SchemaVersion int           `json:"schema_version"`
	GeneratedAt   string        `json:"generated_at"`
	Repeaters     []ReportEntry `json:"repeaters"`
}

// ReportEntry is one node. No omitempty: an unread value must arrive as null.
// Fields the companion protocol has no source for (duplicates, queue-full
// events, temperature, humidity, pressure, battery percent) are left out,
// which the Grid reads as null.
type ReportEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	LastAttempt *int64 `json:"last_attempt"`
	LastSuccess *int64 `json:"last_success"`

	BatteryVoltage *float64 `json:"battery_voltage"`
	UptimeS        *int64   `json:"uptime_s"`
	TxQueueLen     *int32   `json:"tx_queue_len"`
	NoiseFloorDBm  *int32   `json:"noise_floor_dbm"`
	LastRSSIdBm    *int32   `json:"last_rssi_dbm"`
	LastSNRdB      *float64 `json:"last_snr_db"`
	AirtimeMs      *int64   `json:"airtime_ms"`
	RxAirtimeMs    *int64   `json:"rx_airtime_ms"`
	NbSent         *int64   `json:"nb_sent"`
	NbRecv         *int64   `json:"nb_recv"`
	SentFlood      *int64   `json:"sent_flood"`
	SentDirect     *int64   `json:"sent_direct"`
	RecvFlood      *int64   `json:"recv_flood"`
	RecvDirect     *int64   `json:"recv_direct"`
	RecvErrors     *int64   `json:"recv_errors"`
	Online         bool     `json:"online"`
}

// Report builds the complete current set. A node never reached carries no
// readings; a node reached before but failing now keeps its last readings,
// whose age is last_success.
func (p *Poller) Report() Report {
	now := p.cfg.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	rep := Report{SchemaVersion: 1, GeneratedAt: now.UTC().Format(time.RFC3339), Repeaters: []ReportEntry{}}
	gws := make([]string, 0, len(p.nodes))
	for gw := range p.nodes {
		gws = append(gws, gw)
	}
	sort.Strings(gws)
	for _, gw := range gws {
		st := p.nodes[gw]
		if st.lastAttempt.IsZero() {
			continue // only ever skipped: we have not tried it yet
		}
		e := ReportEntry{ID: st.pubkey, Name: st.name, LastAttempt: unix(st.lastAttempt),
			LastSuccess: unix(st.lastSuccess), Online: st.outcome == outcomeOK}
		if s := st.sample; s != nil {
			e.BatteryVoltage, e.UptimeS, e.TxQueueLen = s.BatteryVoltage, s.UptimeS, s.TxQueueLen
			e.NoiseFloorDBm, e.LastRSSIdBm, e.LastSNRdB = s.NoiseFloorDBm, s.LastRSSIdBm, s.LastSNRdB
			e.AirtimeMs, e.RxAirtimeMs = s.AirtimeMs, s.RxAirtimeMs
			e.NbSent, e.NbRecv = s.NbSent, s.NbRecv
			e.SentFlood, e.SentDirect, e.RecvFlood, e.RecvDirect = s.SentFlood, s.SentDirect, s.RecvFlood, s.RecvDirect
			e.RecvErrors = s.RecvErrors
		}
		rep.Repeaters = append(rep.Repeaters, e)
	}
	return rep
}

func unix(t time.Time) *int64 {
	if t.IsZero() {
		return nil
	}
	v := t.Unix()
	return &v
}
