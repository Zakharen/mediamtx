// Package rtpext contains helpers for forwarding RTP header extensions across
// depacketization and repacketization boundaries.
package rtpext

import (
	"strconv"
	"strings"

	"github.com/pion/rtp"
)

const (
	// FrameMetadataURI identifies the per-frame metadata injected by the RTP source.
	FrameMetadataURI = "urn:example:rtp-hdrext:frame-metadata"

	// FrameMetadataSize is 15 bytes of UUID followed by a one-byte frame identifier.
	FrameMetadataSize = 16
)

// ParseExtMap parses the value of an SDP a=extmap attribute.
func ParseExtMap(value string) (uint8, string, bool) {
	parts := strings.Fields(value)
	if len(parts) < 2 {
		return 0, "", false
	}

	idPart, _, _ := strings.Cut(parts[0], "/")
	id64, err := strconv.ParseUint(idPart, 10, 8)
	if err != nil || id64 == 0 {
		return 0, "", false
	}

	return uint8(id64), parts[1], true
}

// ExtractFrameMetadata returns a detached copy of valid frame metadata.
func ExtractFrameMetadata(pkt *rtp.Packet, id uint8) []byte {
	if pkt == nil || id == 0 {
		return nil
	}

	payload := pkt.GetExtension(id)
	if len(payload) != FrameMetadataSize {
		return nil
	}

	return append([]byte(nil), payload...)
}

// Add adds an extension payload to every packet. Existing extensions are preserved.
func Add(packets []*rtp.Packet, id uint8, payload []byte) error {
	if id == 0 || len(payload) == 0 {
		return nil
	}

	for _, pkt := range packets {
		if pkt == nil {
			continue
		}

		err := pkt.SetExtension(id, payload)
		if err != nil {
			return err
		}
	}

	return nil
}
