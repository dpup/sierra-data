package relaytelemetry

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// The MeshCore companion-protocol commands and replies the poller uses
// (examples/companion_radio/MyMesh.cpp in meshcore-dev/MeshCore). Frames are
// payloads only: the relay carries no '<'/'>' header or length.
const (
	cmdAppStart        byte = 0x01
	cmdGetBattStorage  byte = 0x14 // 20
	cmdDeviceQuery     byte = 0x16 // 22
	cmdGetStats        byte = 0x38 // 56, firmware v8+
	respErr            byte = 0x01
	respSelfInfo       byte = 0x05
	respBattStorage    byte = 0x0c // 12
	respDeviceInfo     byte = 0x0d // 13
	respStats          byte = 0x18 // 24
	statsCore          byte = 0
	statsRadio         byte = 1
	statsPackets       byte = 2
	statsMinFirmware        = 8 // CMD_GET_STATS needs firmware ver code 8+
	deviceQueryVersion byte = 3
	appName                 = "sierra-relay-telemetry"
)

const (
	selfInfoPubkeyOff = 4
	pubkeySize        = 32
	selfInfoNameOff   = selfInfoPubkeyOff + pubkeySize + 4 + 4 + 1 + 1 + 1 + 1 + 4 + 4 + 1 + 1 // 58
)

func appStartFrame() []byte {
	return append([]byte{cmdAppStart, 1, 0, 0, 0, 0, 0, 0}, appName...)
}

func deviceQueryFrame() []byte    { return []byte{cmdDeviceQuery, deviceQueryVersion} }
func battStorageFrame() []byte    { return []byte{cmdGetBattStorage} }
func statsFrame(kind byte) []byte { return []byte{cmdGetStats, kind} }
func isCode(code byte) func([]byte) bool {
	return func(f []byte) bool { return len(f) > 0 && f[0] == code }
}

func isStats(kind byte) func([]byte) bool {
	return func(f []byte) bool { return len(f) > 1 && f[0] == respStats && f[1] == kind }
}

// errNodeRefused is a RESP_CODE_ERR answer: the firmware does not support the
// command (or refused it). The value is unread, not zero.
var errNodeRefused = errors.New("node answered ERR")

// parseSelfInfo reads the node's public key and advertised name.
func parseSelfInfo(f []byte) (pubkey, name string, err error) {
	if len(f) < selfInfoPubkeyOff+pubkeySize || f[0] != respSelfInfo {
		return "", "", fmt.Errorf("not a SELF_INFO frame (%d bytes)", len(f))
	}
	pubkey = hex.EncodeToString(f[selfInfoPubkeyOff : selfInfoPubkeyOff+pubkeySize])
	if len(f) > selfInfoNameOff {
		name = cString(f[selfInfoNameOff:])
	}
	return pubkey, name, nil
}

// parseFirmwareVersion reads DEVICE_INFO's firmware version code (byte 1),
// which gates CMD_GET_STATS.
func parseFirmwareVersion(f []byte) (int, error) {
	if len(f) < 2 || f[0] != respDeviceInfo {
		return 0, errors.New("not a DEVICE_INFO frame")
	}
	return int(f[1]), nil
}

// parseBattery reads RESP_CODE_BATT_AND_STORAGE's battery millivolts.
func parseBattery(f []byte) (volts float64, err error) {
	if len(f) < 3 || f[0] != respBattStorage {
		return 0, errors.New("not a BATT_AND_STORAGE frame")
	}
	return float64(binary.LittleEndian.Uint16(f[1:3])) / 1000, nil
}

// applyCoreStats reads STATS_TYPE_CORE: battery mV, uptime s, error flags,
// outbound queue length.
func applyCoreStats(f []byte, s *Sample) error {
	if len(f) < 11 || !isStats(statsCore)(f) {
		return errors.New("short or wrong core stats frame")
	}
	if s.BatteryVoltage == nil {
		s.BatteryVoltage = ptr(float64(binary.LittleEndian.Uint16(f[2:4])) / 1000)
	}
	s.UptimeS = ptr(int64(binary.LittleEndian.Uint32(f[4:8])))
	s.TxQueueLen = ptr(int32(f[10]))
	return nil
}

// applyRadioStats reads STATS_TYPE_RADIO: noise floor, last RSSI, last SNR
// (quarter dB), and TX/RX airtime in whole seconds.
func applyRadioStats(f []byte, s *Sample) error {
	if len(f) < 14 || !isStats(statsRadio)(f) {
		return errors.New("short or wrong radio stats frame")
	}
	s.NoiseFloorDBm = ptr(int32(int16(binary.LittleEndian.Uint16(f[2:4]))))
	s.LastRSSIdBm = ptr(int32(int8(f[4])))
	s.LastSNRdB = ptr(float64(int8(f[5])) / 4)
	s.AirtimeMs = ptr(int64(binary.LittleEndian.Uint32(f[6:10])) * 1000)
	s.RxAirtimeMs = ptr(int64(binary.LittleEndian.Uint32(f[10:14])) * 1000)
	return nil
}

// applyPacketStats reads STATS_TYPE_PACKETS: received, sent, flood/direct each
// way and, when present, receive errors.
func applyPacketStats(f []byte, s *Sample) error {
	if len(f) < 26 || !isStats(statsPackets)(f) {
		return errors.New("short or wrong packet stats frame")
	}
	u := func(off int) *int64 { return ptr(int64(binary.LittleEndian.Uint32(f[off : off+4]))) }
	s.NbRecv, s.NbSent = u(2), u(6)
	s.SentFlood, s.SentDirect = u(10), u(14)
	s.RecvFlood, s.RecvDirect = u(18), u(22)
	if len(f) >= 30 {
		s.RecvErrors = u(26)
	}
	return nil
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.ToValidUTF8(string(b), "")
}

func ptr[T any](v T) *T { return &v }
