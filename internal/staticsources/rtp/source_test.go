package rtp_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	pionrtp "github.com/pion/rtp"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/rtpext"
	"github.com/bluenviron/mediamtx/internal/staticsources/rtp"
	"github.com/bluenviron/mediamtx/internal/test"
)

func multicastCapableInterface(t *testing.T) string {
	intfs, err := net.Interfaces()
	require.NoError(t, err)

	for _, intf := range intfs {
		if (intf.Flags & net.FlagMulticast) != 0 {
			return intf.Name
		}
	}

	t.Errorf("unable to find a multicast IP")
	return ""
}

func TestSourceUDP(t *testing.T) {
	for _, ca := range []string{
		"unicast",
		"multicast",
		"multicast with interface",
		"unicast with source",
	} {
		t.Run(ca, func(t *testing.T) {
			var src string

			switch ca {
			case "unicast":
				src = "udp+rtp://127.0.0.1:9004"

			case "multicast":
				src = "udp+rtp://238.0.0.1:9004"

			case "multicast with interface":
				src = "udp+rtp://238.0.0.1:9004?interface=" + multicastCapableInterface(t)

			case "unicast with source":
				src = "udp+rtp://127.0.0.1:9004?source=127.0.1.1"
			}

			p := &test.StaticSourceParent{}
			p.Initialize()
			defer p.Close()

			so := &rtp.Source{
				ReadTimeout: conf.Duration(10 * time.Second),
				Parent:      p,
			}

			done := make(chan struct{})
			defer func() { <-done }()

			ctx, ctxCancel := context.WithCancel(context.Background())
			defer ctxCancel()

			reloadConf := make(chan *conf.Path)

			go func() {
				so.Run(defs.StaticSourceRunParams{ //nolint:errcheck
					Context:        ctx,
					ResolvedSource: src,
					Conf: &conf.Path{
						RTPSDP: "v=0\n" +
							"o=- 123456789 123456789 IN IP4 192.168.1.100\n" +
							"s=H264 Video Stream\n" +
							"c=IN IP4 192.168.1.100\n" +
							"t=0 0\n" +
							"m=video 5004 RTP/AVP 96\n" +
							"a=rtpmap:96 H264/90000\n" +
							"a=fmtp:96 profile-level-id=42e01e;packetization-mode=1\n",
					},
					ReloadConf: reloadConf,
				})
				close(done)
			}()

			time.Sleep(50 * time.Millisecond)

			var dest string

			switch ca {
			case "unicast":
				dest = "127.0.0.1:9004"

			case "multicast":
				dest = "238.0.0.1:9004"

			case "multicast with interface":
				dest = "238.0.0.1:9004"

			case "unicast with source":
				dest = "127.0.0.1:9004"
			}

			udest, err := net.ResolveUDPAddr("udp", dest)
			require.NoError(t, err)

			var usrc *net.UDPAddr
			if ca == "unicast with source" {
				usrc, err = net.ResolveUDPAddr("udp", "127.0.1.1:9020")
				require.NoError(t, err)
			}

			conn, err := net.DialUDP("udp", usrc, udest)
			require.NoError(t, err)
			defer conn.Close() //nolint:errcheck

			enc := &rtph264.Encoder{
				PayloadType:       96,
				PacketizationMode: 1,
			}
			err = enc.Init()
			require.NoError(t, err)

			pkts, err := enc.Encode([][]byte{
				{5, 1},
			})
			require.NoError(t, err)

			for _, pkt := range pkts {
				var buf []byte
				buf, err = pkt.Marshal()
				require.NoError(t, err)

				_, err = conn.Write(buf)
				require.NoError(t, err)
			}

			<-p.Unit

			require.Equal(t, defs.StaticSourceInfo{}, so.Info())

			// the source must be listening on ReloadConf
			reloadConf <- nil
		})
	}
}

func TestSourceUDPFrameMetadataExtension(t *testing.T) {
	p := &test.StaticSourceParent{}
	p.Initialize()
	defer p.Close()

	so := &rtp.Source{
		ReadTimeout: conf.Duration(10 * time.Second),
		Parent:      p,
	}

	ctx, ctxCancel := context.WithCancel(context.Background())
	defer ctxCancel()
	done := make(chan struct{})
	defer func() { <-done }()

	go func() {
		so.Run(defs.StaticSourceRunParams{ //nolint:errcheck
			Context:        ctx,
			ResolvedSource: "udp+rtp://127.0.0.1:9014",
			Conf: &conf.Path{
				RTPSDP: "v=0\n" +
					"o=- 123456789 123456789 IN IP4 127.0.0.1\n" +
					"s=H264 Video Stream\n" +
					"c=IN IP4 127.0.0.1\n" +
					"t=0 0\n" +
					"m=video 9014 RTP/AVP 96\n" +
					"a=rtpmap:96 H264/90000\n" +
					"a=fmtp:96 profile-level-id=42e01e;packetization-mode=1\n" +
					"a=extmap:1 " + rtpext.FrameMetadataURI + "\n",
			},
		})
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)

	conn, err := net.Dial("udp", "127.0.0.1:9014")
	require.NoError(t, err)
	defer conn.Close()

	metadata := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 99}
	pkt := &pionrtp.Packet{
		Header: pionrtp.Header{
			Version:        2,
			Marker:         true,
			PayloadType:    96,
			SequenceNumber: 1234,
			Timestamp:      90000,
			SSRC:           0x10203040,
		},
		Payload: []byte{0x65, 0x01},
	}
	require.NoError(t, pkt.SetExtension(1, metadata))
	buf, err := pkt.Marshal()
	require.NoError(t, err)
	_, err = conn.Write(buf)
	require.NoError(t, err)

	u := <-p.Unit
	require.Equal(t, metadata, u.RTPHeaderExtensions[rtpext.FrameMetadataURI])
}

func TestSourceUnixSocket(t *testing.T) {
	for _, ca := range []string{
		"relative",
		"absolute",
	} {
		t.Run(ca, func(t *testing.T) {
			var pa string
			if ca == "relative" {
				pa = "test_rtp.sock"
			} else {
				pa = filepath.Join(t.TempDir(), "test_rtp.sock")
			}

			func() {
				p := &test.StaticSourceParent{}
				p.Initialize()
				defer p.Close()

				so := &rtp.Source{
					ReadTimeout: conf.Duration(10 * time.Second),
					Parent:      p,
				}

				done := make(chan struct{})
				defer func() { <-done }()

				ctx, ctxCancel := context.WithCancel(context.Background())
				defer ctxCancel()

				go func() {
					so.Run(defs.StaticSourceRunParams{ //nolint:errcheck
						Context:        ctx,
						ResolvedSource: "unix+rtp://" + pa,
						Conf: &conf.Path{
							RTPSDP: "v=0\n" +
								"o=- 123456789 123456789 IN IP4 192.168.1.100\n" +
								"s=H264 Video Stream\n" +
								"c=IN IP4 192.168.1.100\n" +
								"t=0 0\n" +
								"m=video 5004 RTP/AVP 96\n" +
								"a=rtpmap:96 H264/90000\n" +
								"a=fmtp:96 profile-level-id=42e01e;packetization-mode=1\n",
						},
					})
					close(done)
				}()

				time.Sleep(50 * time.Millisecond)

				_, err := os.Stat(pa)
				require.NoError(t, err)

				conn, err := net.Dial("unix", pa)
				require.NoError(t, err)
				defer conn.Close()

				enc := &rtph264.Encoder{
					PayloadType:       96,
					PacketizationMode: 1,
				}
				err = enc.Init()
				require.NoError(t, err)

				pkts, err := enc.Encode([][]byte{
					{5, 1},
				})
				require.NoError(t, err)

				for _, pkt := range pkts {
					var buf []byte
					buf, err = pkt.Marshal()
					require.NoError(t, err)

					_, err = conn.Write(buf)
					require.NoError(t, err)
				}

				<-p.Unit
			}()

			_, err := os.Stat(pa)
			require.Error(t, err)
		})
	}
}
