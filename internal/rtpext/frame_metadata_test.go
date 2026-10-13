package rtpext

import (
	"testing"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func TestParseExtMap(t *testing.T) {
	id, uri, ok := ParseExtMap("7/sendonly " + FrameMetadataURI)
	require.True(t, ok)
	require.Equal(t, uint8(7), id)
	require.Equal(t, FrameMetadataURI, uri)

	for _, value := range []string{"", "x " + FrameMetadataURI, "0 " + FrameMetadataURI} {
		_, _, ok = ParseExtMap(value)
		require.False(t, ok)
	}
}

func TestExtractFrameMetadata(t *testing.T) {
	payload := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	pkt := &rtp.Packet{Header: rtp.Header{Version: 2}}
	require.NoError(t, pkt.SetExtension(1, payload))

	extracted := ExtractFrameMetadata(pkt, 1)
	require.Equal(t, payload, extracted)
	extracted[0] = 0xff
	require.Equal(t, byte(0), pkt.GetExtension(1)[0])

	require.Nil(t, ExtractFrameMetadata(pkt, 2))

	malformed := &rtp.Packet{Header: rtp.Header{Version: 2}}
	require.NoError(t, malformed.SetExtension(1, []byte{1, 2, 3}))
	require.NotPanics(t, func() {
		require.Nil(t, ExtractFrameMetadata(malformed, 1))
	})
}

func TestAdd(t *testing.T) {
	metadata := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	packets := []*rtp.Packet{
		{Header: rtp.Header{Version: 2}},
		{Header: rtp.Header{Version: 2}},
		{Header: rtp.Header{Version: 2}},
	}

	// Simulate an independently negotiated WebRTC extension such as MID/TWCC.
	require.NoError(t, packets[0].SetExtension(3, []byte{0x12, 0x34}))
	require.NoError(t, Add(packets, 9, metadata))

	for _, pkt := range packets {
		require.Equal(t, metadata, pkt.GetExtension(9))
	}
	require.Equal(t, []byte{0x12, 0x34}, packets[0].GetExtension(3))
}
