// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

//go:build !js

// simulcast demonstrates of how to handle incoming track with multiple simulcast rtp streams and show all them back.
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

// Approximate average bitrates of the q/h/f simulcast layers the browser
// sends, in bits per second. The layer selector only compares them against
// the bandwidth estimate, so tune them to match your encoder settings.
var simulcastLayers = []simulcastLayer{ //nolint:gochecknoglobals
	{rid: "q", bitrate: 200_000},
	{rid: "h", bitrate: 700_000},
	{rid: "f", bitrate: 2_000_000},
}

// nolint:gocognit, cyclop
func main() {
	// Everything below is the Pion WebRTC API! Thanks for using it ❤️.

	// Prepare the configuration
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		panic(err)
	}
	interceptorRegistry := &interceptor.Registry{}

	// Create a Congestion Controller. It estimates how much bandwidth the
	// connection to the browser has, which the layer selector below uses to
	// recover quickly once the network improves. Passing `nil` to
	// gcc.NewSendSideBWE would use defaults; here we seed it with the
	// lowest layer's bitrate so we never start by overshooting.
	congestionController, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		return gcc.NewSendSideBWE(gcc.SendSideBWEInitialBitrate(simulcastLayers[0].bitrate))
	})
	if err != nil {
		panic(err)
	}
	estimatorChan := make(chan cc.BandwidthEstimator, 1)
	congestionController.OnNewPeerConnection(func(id string, estimator cc.BandwidthEstimator) { //nolint: revive
		estimatorChan <- estimator
	})
	interceptorRegistry.Add(congestionController)
	if err = webrtc.ConfigureTWCCHeaderExtensionSender(mediaEngine, interceptorRegistry); err != nil {
		panic(err)
	}
	if err = webrtc.RegisterDefaultInterceptors(mediaEngine, interceptorRegistry); err != nil {
		panic(err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithInterceptorRegistry(interceptorRegistry))

	// Create a new RTCPeerConnection
	peerConnection, err := api.NewPeerConnection(config)
	if err != nil {
		panic(err)
	}
	defer func() {
		if cErr := peerConnection.Close(); cErr != nil {
			fmt.Printf("cannot close peerConnection: %v\n", cErr)
		}
	}()

	outputTracks := map[string]*webrtc.TrackLocalStaticRTP{}

	// Create Track that we send video back to browser on
	outputTrack, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeVP8,
	}, "video_q", "pion_q")
	if err != nil {
		panic(err)
	}
	outputTracks["q"] = outputTrack

	outputTrack, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeVP8,
	}, "video_h", "pion_h")
	if err != nil {
		panic(err)
	}
	outputTracks["h"] = outputTrack

	outputTrack, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeVP8,
	}, "video_f", "pion_f")
	if err != nil {
		panic(err)
	}
	outputTracks["f"] = outputTrack

	if _, err = peerConnection.AddTransceiverFromKind(
		webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly},
	); err != nil {
		panic(err)
	}

	// Add this newly created track to the PeerConnection to send back video
	if _, err = peerConnection.AddTransceiverFromTrack(
		outputTracks["q"], webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly}); err != nil {
		panic(err)
	}
	if _, err = peerConnection.AddTransceiverFromTrack(
		outputTracks["h"],
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly},
	); err != nil {
		panic(err)
	}
	if _, err = peerConnection.AddTransceiverFromTrack(
		outputTracks["f"],
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly},
	); err != nil {
		panic(err)
	}

	// Read incoming RTCP packets
	// Before these packets are returned they are processed by interceptors. For things
	// like NACK this needs to be called.
	processRTCP := func(rtpSender *webrtc.RTPSender) {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
				return
			}
		}
	}
	for _, rtpSender := range peerConnection.GetSenders() {
		go processRTCP(rtpSender)
	}

	// The layerSelector decides which of the incoming simulcast layers we
	// relay back to the browser. It is driven by packet loss (react fast,
	// protect latency) and the congestion controller's bandwidth estimate
	// (recover quickly without oscillating).
	selector := newLayerSelector(simulcastLayers)

	// SSRCs of the incoming simulcast streams, used to request a keyframe
	// when we switch to a higher layer.
	var trackSSRCsLock sync.Mutex
	trackSSRCs := map[string]uint32{}

	// Re-evaluate the selected layer a few times per second. This adds no
	// latency to the media path: downgrades take effect on the very next
	// packet, and upgrades only wait for the selector's dwell period.
	go func() {
		var estimator cc.BandwidthEstimator
		previousLayer := 0
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			select {
			case estimator = <-estimatorChan:
			default:
			}
			bandwidthEstimate := 0
			if estimator != nil {
				bandwidthEstimate = estimator.GetTargetBitrate()
			}

			lossRatio := maxFractionLost(peerConnection.GetStats())
			layer := selector.update(lossRatio, bandwidthEstimate, time.Now())
			if layer == previousLayer {
				continue
			}
			fmt.Printf("Switching to layer %q (loss=%.2f, estimate=%d bps)\n",
				simulcastLayers[layer].rid, lossRatio, bandwidthEstimate)

			if layer > previousLayer {
				// Ask for a keyframe of the newly selected layer so the
				// browser doesn't sit on a frozen frame until the next
				// periodic keyframe arrives.
				trackSSRCsLock.Lock()
				ssrc, ok := trackSSRCs[simulcastLayers[layer].rid]
				trackSSRCsLock.Unlock()
				if ok {
					if writeErr := peerConnection.WriteRTCP(
						[]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}},
					); writeErr != nil {
						fmt.Println(writeErr)
					}
				}
			}
			previousLayer = layer
		}
	}()

	// Wait for the offer to be pasted
	offer := webrtc.SessionDescription{}
	decode(readUntilNewline(), &offer)

	if err = peerConnection.SetRemoteDescription(offer); err != nil {
		panic(err)
	}

	// Set a handler for when a new remote track starts
	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) { //nolint: revive
		fmt.Println("Track has started")
		trackSSRCsLock.Lock()
		trackSSRCs[track.RID()] = uint32(track.SSRC())
		trackSSRCsLock.Unlock()

		// Start reading from all the streams and sending them to the related output track
		rid := track.RID()
		if track.Kind() == webrtc.RTPCodecTypeVideo {
			go func() {
				ticker := time.NewTicker(3 * time.Second)
				defer ticker.Stop()
				for range ticker.C {
					fmt.Printf("Sending pli for stream with rid: %q, ssrc: %d\n", track.RID(), track.SSRC())
					if writeErr := peerConnection.WriteRTCP(
						[]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}},
					); writeErr != nil {
						fmt.Println(writeErr)
					}
				}
			}()
		}
		for {
			// Read RTP packets being sent to Pion
			packet, _, readErr := track.ReadRTP()
			if readErr != nil {
				panic(readErr)
			}

			// Only forward the layer the selector currently allows. Packets
			// of other layers are dropped here, before they can queue up.
			if rid != selector.currentRID() {
				continue
			}

			if writeErr := outputTracks[rid].WriteRTP(packet); writeErr != nil && !errors.Is(writeErr, io.ErrClosedPipe) {
				panic(writeErr)
			}
		}
	})

	// Set the handler for Peer connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		fmt.Printf("Peer Connection State has changed: %s\n", state.String())

		if state == webrtc.PeerConnectionStateFailed {
			// Wait until PeerConnection has had no network activity for 30 seconds or another failure.
			// It may be reconnected using an ICE Restart.
			// Use webrtc.PeerConnectionStateDisconnected if you are interested in detecting faster timeout.
			// Note that the PeerConnection may come back from PeerConnectionStateDisconnected.
			fmt.Println("Peer Connection has gone to failed exiting")
			os.Exit(0)
		}

		if state == webrtc.PeerConnectionStateClosed {
			// PeerConnection was explicitly closed. This usually happens from a DTLS CloseNotify
			fmt.Println("Peer Connection has gone to closed exiting")
			os.Exit(0)
		}
	})

	// Create an answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	// Sets the LocalDescription, and starts our UDP listeners
	err = peerConnection.SetLocalDescription(answer)
	if err != nil {
		panic(err)
	}

	// Block until ICE Gathering is complete, disabling trickle ICE
	// we do this because we only can exchange one signaling message
	// in a production application you should exchange ICE Candidates via OnICECandidate
	<-gatherComplete

	// Output the answer in base64 so we can paste it in browser
	fmt.Println(encode(peerConnection.LocalDescription()))

	// Block forever
	select {}
}

// maxFractionLost returns the highest fraction-lost value across all
// outbound streams, as reported by the browser in RTCP Receiver Reports.
func maxFractionLost(report webrtc.StatsReport) float64 {
	maxLoss := 0.0
	for _, stats := range report {
		if inbound, ok := stats.(webrtc.RemoteInboundRTPStreamStats); ok && inbound.FractionLost > maxLoss {
			maxLoss = inbound.FractionLost
		}
	}

	return maxLoss
}

// Read from stdin until we get a newline.
func readUntilNewline() (in string) {
	var err error

	r := bufio.NewReader(os.Stdin)
	for {
		in, err = r.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			panic(err)
		}

		if in = strings.TrimSpace(in); len(in) > 0 {
			break
		}
	}

	fmt.Println("")

	return
}

// JSON encode + base64 a SessionDescription.
func encode(obj *webrtc.SessionDescription) string {
	b, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}

	return base64.StdEncoding.EncodeToString(b)
}

// Decode a base64 and unmarshal JSON into a SessionDescription.
func decode(in string, obj *webrtc.SessionDescription) {
	b, err := base64.StdEncoding.DecodeString(in)
	if err != nil {
		panic(err)
	}

	if err = json.Unmarshal(b, obj); err != nil {
		panic(err)
	}
}
