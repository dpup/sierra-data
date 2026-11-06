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
	// Shared repeaters only (from their status and telemetry replies).
	TemperatureC *float64
	DirectDups   *int64
	FloodDups    *int64
}

// Report is the mesh.repeater payload (docs/mesh-reporter-guide.md), the same
// document alanpi sends.
type Report struct {
	SchemaVersion int           `json:"schema_version"`
	GeneratedAt   string        `json:"generated_at"`
	Repeaters     []ReportEntry `json:"repeaters"`
}

// ReportEntry is one node. No omitempty: an unread value must arrive as null.
// Fields neither the companion nor a repeater's status has a source for
// (queue-full events, humidity, pressure, battery percent) are left out,
// which the Grid reads as null. A gateway node has no temperature or
// duplicate counts either; they're sent as null.
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
	TemperatureC   *float64 `json:"temperature_c"`
	DirectDups     *int64   `json:"direct_dups"`
	FloodDups      *int64   `json:"flood_dups"`
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
	var all []*nodeState
	gws := make([]string, 0, len(p.nodes))
	for gw := range p.nodes {
		gws = append(gws, gw)
	}
	sort.Strings(gws)
	for _, gw := range gws {
		all = append(all, p.nodes[gw])
	}
	pks := make([]string, 0, len(p.repeaters))
	for pk := range p.repeaters {
		pks = append(pks, pk)
	}
	sort.Strings(pks)
	for _, pk := range pks {
		all = append(all, p.repeaters[pk])
	}
	for _, st := range all {
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
			e.TemperatureC, e.DirectDups, e.FloodDups = s.TemperatureC, s.DirectDups, s.FloodDups
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
