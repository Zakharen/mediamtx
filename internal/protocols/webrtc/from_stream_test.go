package webrtc_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/protocols/webrtc"
	"github.com/bluenviron/mediamtx/internal/rtpext"
	"github.com/bluenviron/mediamtx/internal/stream"
	"github.com/bluenviron/mediamtx/internal/test"
	"github.com/bluenviron/mediamtx/internal/unit"
)

func TestFromStreamNoSupportedCodecs(t *testing.T) {
	desc := &description.Session{Medias: []*description.Media{{
		Type:    description.MediaTypeVideo,
		Formats: []format.Format{&format.MJPEG{}},
	}}}

	r := &stream.Reader{
		Parent: test.Logger(func(logger.Level, string, ...any) {
			t.Error("should not happen")
		}),
	}

	pc := &webrtc.PeerConnection{}

	err := webrtc.FromStream(desc, r, pc)
	require.ErrorContains(t, err, "the stream doesn't contain any supported codec")
}

func TestFromStreamSkipUnsupportedTracks(t *testing.T) {
	desc := &description.Session{Medias: []*description.Media{
		{
			Type:    description.MediaTypeVideo,
			Formats: []format.Format{&format.H264{PacketizationMode: 1}},
		},
		{
			Type:    description.MediaTypeVideo,
			Formats: []format.Format{&format.MJPEG{}},
		},
	}}

	n := 0

	r := &stream.Reader{
		Parent: test.Logger(func(l logger.Level, format string, args ...any) {
			require.Equal(t, logger.Warn, l)
			if n == 0 {
				require.Equal(t, "skipping track 2 (M-JPEG)", fmt.Sprintf(format, args...))
			}
			n++
		}),
	}

	pc := &webrtc.PeerConnection{}

	err := webrtc.FromStream(desc, r, pc)
	require.NoError(t, err)

	require.Equal(t, 1, n)
}

func TestFromStream(t *testing.T) {
	for _, ca := range toFromStreamCases {
		t.Run(ca.name, func(t *testing.T) {
			desc := &description.Session{
				Medias: []*description.Media{{
					Formats: []format.Format{ca.in},
				}},
			}

			pc := &webrtc.PeerConnection{}
			r := &stream.Reader{Parent: test.NilLogger}

			err := webrtc.FromStream(desc, r, pc)
			require.NoError(t, err)

			require.Equal(t, ca.webrtcCaps, pc.OutboundTracks[0].Caps)
		})
	}
}

func extensionIDFromSDP(t *testing.T, raw string, targetURI string) uint8 {
	var sd sdp.SessionDescription
	require.NoError(t, sd.Unmarshal([]byte(raw)))

	for _, media := range sd.MediaDescriptions {
		if media.MediaName.Media != "video" {
			continue
		}
		for _, attr := range media.Attributes {
			if attr.Key != "extmap" {
				continue
			}
			id, uri, ok := rtpext.ParseExtMap(attr.Value)
			if ok && uri == targetURI {
				return id
			}
		}
	}

	t.Fatalf("extension was not advertised: %s", targetURI)
	return 0
}

func TestFromStreamFrameMetadata(t *testing.T) {
	for _, ca := range []struct {
		name    string
		format  format.Format
		payload unit.Payload
	}{
		{
			name:   "H264",
			format: test.FormatH264,
			payload: unit.PayloadH264{
				append([]byte{0x05}, make([]byte, 3000)...),
			},
		},
		{
			name:   "H265",
			format: &format.H265{PayloadTyp: 96},
			payload: unit.PayloadH265{
				append([]byte{38, 1}, make([]byte, 3000)...),
			},
		},
	} {
		t.Run(ca.name, func(t *testing.T) {
			desc := &description.Session{Medias: []*description.Media{{
				Type:    description.MediaTypeVideo,
				Formats: []format.Format{ca.format},
			}}}
			strm := &stream.Stream{
				OrigDesc:          desc,
				WriteQueueSize:    512,
				RTPMaxPayloadSize: 1450,
				Parent:            test.NilLogger,
			}
			require.NoError(t, strm.Initialize())
			t.Cleanup(strm.Close)

			subStream := &stream.SubStream{Stream: strm, UseRTPPackets: false}
			require.NoError(t, subStream.Initialize())

			readerPC := &webrtc.PeerConnection{
				LocalRandomUDP: true, IPsFromInterfaces: true,
				Publish: false, Log: test.NilLogger,
			}
			require.NoError(t, readerPC.Start())
			t.Cleanup(readerPC.Close)

			publisherPC := &webrtc.PeerConnection{
				LocalRandomUDP: true, IPsFromInterfaces: true,
				Publish: true, Log: test.NilLogger,
			}
			r := &stream.Reader{Parent: test.NilLogger}
			require.NoError(t, webrtc.FromStream(desc, r, publisherPC))
			require.NoError(t, publisherPC.Start())
			t.Cleanup(publisherPC.Close)

			offer, err := readerPC.CreatePartialOffer(false)
			require.NoError(t, err)
			outputID := extensionIDFromSDP(t, offer.SDP, rtpext.FrameMetadataURI)
			require.NotEqual(t, uint8(1), outputID)
			twccID := extensionIDFromSDP(t, offer.SDP,
				"http://www.ietf.org/id/draft-holmer-rmcat-transport-wide-cc-extensions-01")
			require.NotEqual(t, outputID, twccID)

			answer, err := publisherPC.CreateFullAnswer(offer, false)
			require.NoError(t, err)
			require.NoError(t, readerPC.SetAnswer(answer))
			require.NoError(t, readerPC.WaitUntilConnected(10*time.Second))
			require.NoError(t, publisherPC.WaitUntilConnected(10*time.Second))

			strm.AddReader(r)
			t.Cleanup(func() { strm.RemoveReader(r) })

			writeUnit := func(pts int64, extensions map[string][]byte) {
				subStream.WriteUnit(desc.Medias[0], ca.format, &unit.Unit{
					PTS:                 pts,
					NTP:                 time.Now(),
					RTPHeaderExtensions: extensions,
					Payload:             ca.payload,
				})
			}

			writeUnit(0, nil)
			require.NoError(t, readerPC.GatherInboundTracks(2*time.Second))
			tracks := readerPC.InboundTracks()
			require.Len(t, tracks, 1)

			metadata := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 77}
			done := make(chan struct{})
			errCh := make(chan string, 1)
			packetCount := 0
			var targetTimestamp uint32
			tracking := false
			tracks[0].OnPacketRTP = func(pkt *rtp.Packet) {
				got := pkt.GetExtension(outputID)
				if !tracking {
					if len(got) == 0 {
						return
					}
					tracking = true
					targetTimestamp = pkt.Timestamp
				}
				if pkt.Timestamp != targetTimestamp {
					return
				}
				if string(got) != string(metadata) {
					select {
					case errCh <- fmt.Sprintf("packet %d has metadata %x", packetCount, got):
					default:
					}
					return
				}
				packetCount++
				if pkt.Marker {
					close(done)
				}
			}
			readerPC.StartReading()

			writeUnit(90000, map[string][]byte{rtpext.FrameMetadataURI: metadata})

			select {
			case <-done:
			case msg := <-errCh:
				t.Fatal(msg)
			case <-time.After(10 * time.Second):
				t.Fatal("did not receive frame metadata")
			}
			require.Greater(t, packetCount, 1)
		})
	}
}

func TestFromStreamResampleAudio(t *testing.T) {
	for _, ca := range []struct {
		name            string
		format          format.Format
		payloadType     uint8
		payload         []byte
		step            time.Duration
		expectedTSDelta uint32
	}{
		{
			name: "opus stereo",
			format: &format.Opus{
				ChannelCount: 2,
			},
			payloadType:     111,
			payload:         []byte{1},
			step:            20 * time.Millisecond,
			expectedTSDelta: 960,
		},
		{
			name: "g711 pcma 8khz mono",
			format: &format.G711{
				PayloadTyp:   8,
				SampleRate:   8000,
				ChannelCount: 1,
			},
			payloadType:     8,
			payload:         make([]byte, 160),
			step:            20 * time.Millisecond,
			expectedTSDelta: 160,
		},
		{
			name: "g711 pcmu 16khz stereo",
			format: &format.G711{
				MULaw:        true,
				PayloadTyp:   96,
				SampleRate:   16000,
				ChannelCount: 2,
			},
			payloadType:     96,
			payload:         make([]byte, 320),
			step:            10 * time.Millisecond,
			expectedTSDelta: 160,
		},
		{
			name: "lpcm 16khz stereo",
			format: &format.LPCM{
				PayloadTyp:   96,
				BitDepth:     16,
				SampleRate:   16000,
				ChannelCount: 2,
			},
			payloadType:     96,
			payload:         make([]byte, 640),
			step:            10 * time.Millisecond,
			expectedTSDelta: 160,
		},
	} {
		t.Run(ca.name, func(t *testing.T) {
			strm := &stream.Stream{
				OrigDesc: &description.Session{Medias: []*description.Media{{
					Type:    description.MediaTypeAudio,
					Formats: []format.Format{ca.format},
				}}},
				WriteQueueSize:    512,
				RTPMaxPayloadSize: 1450,
				ReplaceNTP:        false,
				Parent:            test.NilLogger,
			}
			err := strm.Initialize()
			require.NoError(t, err)
			t.Cleanup(strm.Close)

			subStream := &stream.SubStream{
				Stream:        strm,
				UseRTPPackets: true,
			}
			err = subStream.Initialize()
			require.NoError(t, err)

			pcReader := &webrtc.PeerConnection{
				LocalRandomUDP:    true,
				IPsFromInterfaces: true,
				Publish:           false,
				Log:               test.NilLogger,
			}
			err = pcReader.Start()
			require.NoError(t, err)
			t.Cleanup(pcReader.Close)

			pcPublisher := &webrtc.PeerConnection{
				LocalRandomUDP:    true,
				IPsFromInterfaces: true,
				Publish:           true,
				Log:               test.NilLogger,
			}

			r := &stream.Reader{Parent: nil}

			err = webrtc.FromStream(strm.OrigDesc, r, pcPublisher)
			require.NoError(t, err)

			err = pcPublisher.Start()
			require.NoError(t, err)
			t.Cleanup(pcPublisher.Close)

			offer, err := pcReader.CreatePartialOffer(false)
			require.NoError(t, err)

			answer, err := pcPublisher.CreateFullAnswer(offer, false)
			require.NoError(t, err)

			err = pcReader.SetAnswer(answer)
			require.NoError(t, err)

			err = pcReader.WaitUntilConnected(10 * time.Second)
			require.NoError(t, err)

			err = pcPublisher.WaitUntilConnected(10 * time.Second)
			require.NoError(t, err)

			strm.AddReader(r)
			t.Cleanup(func() { strm.RemoveReader(r) })

			baseNTP := time.Unix(1710000000, 0)
			step := ca.step
			const initialTimestamp = uint32(45343)

			makeUnit := func(seq uint16, ntp time.Time) *unit.Unit {
				return &unit.Unit{
					PTS: 0,
					NTP: ntp,
					RTPPackets: []*rtp.Packet{{
						Version:        2,
						Marker:         true,
						PayloadType:    ca.payloadType,
						SequenceNumber: seq,
						Timestamp:      initialTimestamp,
						SSRC:           563424,
						Payload:        append([]byte(nil), ca.payload...),
					}},
				}
			}

			// prime the pipeline to allow track gathering
			subStream.WriteUnit(strm.OrigDesc.Medias[0], strm.OrigDesc.Medias[0].Formats[0],
				makeUnit(1123, baseNTP))

			err = pcReader.GatherInboundTracks(2 * time.Second)
			require.NoError(t, err)

			tracks := pcReader.InboundTracks()
			require.Len(t, tracks, 1)

			done := make(chan struct{})
			errCh := make(chan string, 1)
			const startSeq = uint16(2000)

			var recvIndex int
			var prevTS uint32
			prevTSValid := false
			sawTSDelta := false
			sawNTP := false

			tracks[0].OnPacketRTP = func(pkt *rtp.Packet) {
				if prevTSValid {
					if pkt.Timestamp-prevTS != ca.expectedTSDelta {
						select {
						case errCh <- fmt.Sprintf("timestamp delta mismatch for packet=%d: got=%d expected=%d",
							recvIndex, pkt.Timestamp-prevTS, ca.expectedTSDelta):
						default:
						}
						return
					}
					sawTSDelta = true
				}
				prevTS = pkt.Timestamp
				prevTSValid = true

				ntp, avail := tracks[0].PacketNTP(pkt)
				if avail {
					expected := baseNTP.Add(time.Duration(recvIndex) * step)
					if ntp.Sub(expected).Abs() > 50*time.Millisecond {
						select {
						case errCh <- fmt.Sprintf("absolute NTP mismatch for packet=%d: got=%v expected=%v",
							recvIndex, ntp, expected):
						default:
						}
						return
					}
					sawNTP = true
				}

				recvIndex++

				if sawTSDelta && sawNTP {
					select {
					case done <- struct{}{}:
					default:
					}
				}
			}

			pcReader.StartReading()

			go func() {
				ticker := time.NewTicker(step)
				defer ticker.Stop()

				for i := range uint16(150) {
					seq := startSeq + i
					expected := baseNTP.Add(time.Duration(i) * step)

					subStream.WriteUnit(strm.OrigDesc.Medias[0], strm.OrigDesc.Medias[0].Formats[0],
						makeUnit(seq, expected))

					<-ticker.C
				}
			}()

			select {
			case <-done:
			case err := <-errCh:
				t.Fatal(err)
			case <-time.After(8 * time.Second):
				t.Fatal("audio timestamp mapping did not become available")
			}
		})
	}
}

func TestFromStreamFilterH264SEIUserDataUnregistered(t *testing.T) {
	strm := &stream.Stream{
		OrigDesc: &description.Session{Medias: []*description.Media{{
			Type:    description.MediaTypeVideo,
			Formats: []format.Format{test.FormatH264},
		}}},
		WriteQueueSize:    512,
		RTPMaxPayloadSize: 1450,
		Parent:            test.NilLogger,
	}
	err := strm.Initialize()
	require.NoError(t, err)
	t.Cleanup(strm.Close)

	subStream := &stream.SubStream{
		Stream:        strm,
		UseRTPPackets: false,
	}
	err = subStream.Initialize()
	require.NoError(t, err)

	pcReader := &webrtc.PeerConnection{
		LocalRandomUDP:    true,
		IPsFromInterfaces: true,
		Publish:           false,
		Log:               test.NilLogger,
	}
	err = pcReader.Start()
	require.NoError(t, err)
	t.Cleanup(pcReader.Close)

	pcPublisher := &webrtc.PeerConnection{
		LocalRandomUDP:    true,
		IPsFromInterfaces: true,
		Publish:           true,
		Log:               test.NilLogger,
	}

	r := &stream.Reader{Parent: test.NilLogger}
	err = webrtc.FromStream(strm.OrigDesc, r, pcPublisher)
	require.NoError(t, err)

	err = pcPublisher.Start()
	require.NoError(t, err)
	t.Cleanup(pcPublisher.Close)

	offer, err := pcReader.CreatePartialOffer(false)
	require.NoError(t, err)

	answer, err := pcPublisher.CreateFullAnswer(offer, false)
	require.NoError(t, err)

	err = pcReader.SetAnswer(answer)
	require.NoError(t, err)

	err = pcReader.WaitUntilConnected(10 * time.Second)
	require.NoError(t, err)

	err = pcPublisher.WaitUntilConnected(10 * time.Second)
	require.NoError(t, err)

	strm.AddReader(r)
	t.Cleanup(func() { strm.RemoveReader(r) })

	writeUnit := func(au unit.PayloadH264) {
		subStream.WriteUnit(strm.OrigDesc.Medias[0], strm.OrigDesc.Medias[0].Formats[0], &unit.Unit{
			NTP:     time.Now(),
			Payload: au,
		})
	}

	writeUnit(unit.PayloadH264{{5, 2, 3, 4}})

	err = pcReader.GatherInboundTracks(2 * time.Second)
	require.NoError(t, err)

	tracks := pcReader.InboundTracks()
	require.Len(t, tracks, 1)

	done := make(chan struct{})
	var once sync.Once
	var decoder rtph264.Decoder
	err = decoder.Init()
	require.NoError(t, err)

	var receivedAU [][]byte
	tracks[0].OnPacketRTP = func(pkt *rtp.Packet) {
		au, err2 := decoder.Decode(pkt)
		if err2 != nil || au == nil {
			return
		}

		for _, nalu := range au {
			if len(nalu) >= 2 && nalu[0] == 6 && nalu[1] == 245 {
				receivedAU = au
				once.Do(func() { close(done) })
			}
		}
	}
	pcReader.StartReading()

	writeUnit(unit.PayloadH264{
		{
			6, 5, 16,
			0x81, 0x6d, 0x38, 0x4e, 0x99, 0x8c, 0x11, 0xea,
			0xb2, 0x94, 0x02, 0xfc, 0xdc, 0x4e, 0x74, 0x12,
		},
		{
			6, 5, 16,
			0x00, 0x6d, 0x38, 0x4e, 0x99, 0x8c, 0x11, 0xea,
			0xb2, 0x94, 0x02, 0xfc, 0xdc, 0x4e, 0x74, 0x12,
		},
		{6, 245, 1, 0x01},
		{5, 2, 3, 4},
	})

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("did not receive H264 access unit")
	}

	require.Equal(t, [][]byte{
		test.FormatH264.SPS,
		test.FormatH264.PPS,
		{
			6, 5, 16,
			0x00, 0x6d, 0x38, 0x4e, 0x99, 0x8c, 0x11, 0xea,
			0xb2, 0x94, 0x02, 0xfc, 0xdc, 0x4e, 0x74, 0x12,
		},
		{6, 245, 1, 0x01},
		{5, 2, 3, 4},
	}, receivedAU)
}

func TestFromStreamDoesNotMutateSharedRTPPackets(t *testing.T) {
	for _, ca := range []struct {
		name        string
		format      format.Format
		payloadType uint8
		payload     []byte
	}{
		{
			name: "opus stereo",
			format: &format.Opus{
				ChannelCount: 2,
			},
			payloadType: 111,
			payload:     []byte{1},
		},
		{
			name:        "g722",
			format:      &format.G722{},
			payloadType: 9,
			payload:     []byte{1, 2, 3, 4},
		},
		{
			name: "g711 pcma 8khz mono",
			format: &format.G711{
				PayloadTyp:   8,
				SampleRate:   8000,
				ChannelCount: 1,
			},
			payloadType: 8,
			payload:     []byte{1, 2, 3, 4},
		},
		{
			name: "g711 pcmu 8khz mono",
			format: &format.G711{
				MULaw:        true,
				PayloadTyp:   0,
				SampleRate:   8000,
				ChannelCount: 1,
			},
			payloadType: 0,
			payload:     []byte{1, 2, 3, 4},
		},
	} {
		t.Run(ca.name, func(t *testing.T) {
			strm := &stream.Stream{
				OrigDesc: &description.Session{Medias: []*description.Media{{
					Type:    description.MediaTypeAudio,
					Formats: []format.Format{ca.format},
				}}},
				WriteQueueSize:    512,
				RTPMaxPayloadSize: 1450,
				ReplaceNTP:        false,
				Parent:            test.NilLogger,
			}
			err := strm.Initialize()
			require.NoError(t, err)
			t.Cleanup(strm.Close)

			subStream := &stream.SubStream{
				Stream:        strm,
				UseRTPPackets: true,
			}
			err = subStream.Initialize()
			require.NoError(t, err)

			pcReader := &webrtc.PeerConnection{
				LocalRandomUDP:    true,
				IPsFromInterfaces: true,
				Publish:           false,
				Log:               test.NilLogger,
			}
			err = pcReader.Start()
			require.NoError(t, err)
			t.Cleanup(pcReader.Close)

			pcPublisher := &webrtc.PeerConnection{
				LocalRandomUDP:    true,
				IPsFromInterfaces: true,
				Publish:           true,
				Log:               test.NilLogger,
			}

			r := &stream.Reader{Parent: test.NilLogger}

			err = webrtc.FromStream(strm.OrigDesc, r, pcPublisher)
			require.NoError(t, err)

			err = pcPublisher.Start()
			require.NoError(t, err)
			t.Cleanup(pcPublisher.Close)

			const originalSSRC = uint32(563424)
			require.NotEmpty(t, pcPublisher.OutboundTracks)

			offer, err := pcReader.CreatePartialOffer(false)
			require.NoError(t, err)

			answer, err := pcPublisher.CreateFullAnswer(offer, false)
			require.NoError(t, err)

			err = pcReader.SetAnswer(answer)
			require.NoError(t, err)

			err = pcReader.WaitUntilConnected(10 * time.Second)
			require.NoError(t, err)

			err = pcPublisher.WaitUntilConnected(10 * time.Second)
			require.NoError(t, err)

			strm.AddReader(r)
			t.Cleanup(func() { strm.RemoveReader(r) })

			makeUnit := func(seq uint16) *unit.Unit {
				return &unit.Unit{
					PTS: 0,
					NTP: time.Now(),
					RTPPackets: []*rtp.Packet{{
						Version:        2,
						Marker:         true,
						PayloadType:    ca.payloadType,
						SequenceNumber: seq,
						Timestamp:      45343,
						SSRC:           originalSSRC,
						Payload:        append([]byte(nil), ca.payload...),
					}},
				}
			}

			// prime the pipeline to allow track gathering
			subStream.WriteUnit(strm.OrigDesc.Medias[0], strm.OrigDesc.Medias[0].Formats[0], makeUnit(1123))

			err = pcReader.GatherInboundTracks(2 * time.Second)
			require.NoError(t, err)

			tracks := pcReader.InboundTracks()
			require.Len(t, tracks, 1)

			done := make(chan struct{})
			n := 0
			tracks[0].OnPacketRTP = func(_ *rtp.Packet) {
				n++
				if n == 2 {
					close(done)
				}
			}

			pcReader.StartReading()

			testUnit := makeUnit(1124)
			subStream.WriteUnit(strm.OrigDesc.Medias[0], strm.OrigDesc.Medias[0].Formats[0], testUnit)

			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("did not receive packet")
			}

			require.Equal(t, originalSSRC, testUnit.RTPPackets[0].SSRC)
		})
	}
}
