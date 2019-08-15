package gumbleopenal // import "github.com/bmmcginty/gumble/gumbleopenal"

import (
	"encoding/binary"
	"errors"
	"os/exec"
	"time"

	"github.com/bmmcginty/gumble/gumble"
	"github.com/timshannon/go-openal/openal"
)

var (
	ErrState = errors.New("gumbleopenal: invalid state")
	ErrMic   = errors.New("gumbleopenal: microphone disconnected or misconfigured")
)

func beep() {
	cmd := exec.Command("beep")
	cmdout, err := cmd.Output()
	if err != nil {
		panic(err)
	}
	if cmdout != nil {
	}
}

type Stream struct {
	client *gumble.Client
	link   gumble.Detacher

	deviceSource    *openal.CaptureDevice
	sourceFrameSize int
	micVolume       float32
	sourceStop      chan bool

	deviceSink  *openal.Device
	contextSink *openal.Context
}

func New(client *gumble.Client, inputDevice *string, outputDevice *string) (*Stream, error) {
	s := &Stream{
		client:          client,
		sourceFrameSize: client.Config.AudioFrameSize(),
	}

	s.deviceSource = openal.CaptureOpenDevice(*inputDevice, gumble.AudioSampleRate, openal.FormatMono16, uint32(s.sourceFrameSize))
	if s.deviceSource == nil {
	}

	s.deviceSink = openal.OpenDevice(*outputDevice)
	s.contextSink = s.deviceSink.CreateContext()
	s.contextSink.Activate()

	s.link = client.Config.AttachAudio(s)

	return s, nil
}

func (s *Stream) Destroy() {
	s.link.Detach()
	if s.deviceSource != nil {
		if s.deviceSource != nil {
			s.StopSource()
			s.deviceSource.CaptureCloseDevice()
		}
		s.deviceSource = nil
	}
	if s.deviceSink != nil {
		s.contextSink.Destroy()
		s.deviceSink.CloseDevice()
		s.contextSink = nil
		s.deviceSink = nil
	}
}

func (s *Stream) StartSource(inputDevice *string) error {
	if s.sourceStop != nil {
		return ErrState
	}
	if s.deviceSource == nil {
		return ErrMic
	} else {
		s.deviceSource.CaptureStart()
		s.sourceStop = make(chan bool)
		go s.sourceRoutine(inputDevice)
	}
	return nil
}

func (s *Stream) StopSource() error {
	if s.deviceSource == nil {
		return ErrMic
	}
	s.deviceSource.CaptureStop()
	if s.sourceStop == nil {
		return ErrState
	}
	close(s.sourceStop)
	s.sourceStop = nil
	return nil
}

func (s *Stream) GetMicVolume() float32 {
	return s.micVolume
	//deviceSource.GetGain()
}

func (s *Stream) SetMicVolume(change float32, relative bool) {
	var val float32
	if relative {
		val = s.GetMicVolume() + change
	} else {
		val = change
	}
	if val >= 1 {
		val = 1.0
	}
	if val <= 0 {
		val = 0
	}
	s.micVolume = val
}

func (s *Stream) OnAudioStream(e *gumble.AudioStreamEvent) {
	go func(e *gumble.AudioStreamEvent) {
		var source = openal.NewSource()
		e.User.AudioSource = &source
		e.User.AudioSource.SetGain(e.User.Volume)
		//source := e.User.AudioSource
		emptyBufs := openal.NewBuffers(8)
		reclaim := func() {
			if n := source.BuffersProcessed(); n > 0 {
				reclaimedBufs := make(openal.Buffers, n)
				source.UnqueueBuffers(reclaimedBufs)
				emptyBufs = append(emptyBufs, reclaimedBufs...)
			}
		}
		var raw [gumble.AudioMaximumFrameSize * 2]byte
		for packet := range e.C {
			samples := len(packet.AudioBuffer)
			if samples > cap(raw) {
				continue
			}
			for i, value := range packet.AudioBuffer {
				binary.LittleEndian.PutUint16(raw[i*2:], uint16(value))
			}
			reclaim()
			if len(emptyBufs) == 0 {
				continue
			}
			last := len(emptyBufs) - 1
			buffer := emptyBufs[last]
			emptyBufs = emptyBufs[:last]
			buffer.SetData(openal.FormatMono16, raw[:samples*2], gumble.AudioSampleRate)
			source.QueueBuffer(buffer)
			if source.State() != openal.Playing {
				source.Play()
			}
		}
		reclaim()
		emptyBufs.Delete()
		source.Delete()
	}(e)
}

func (s *Stream) sourceRoutine(inputDevice *string) {
	interval := s.client.Config.AudioInterval
	frameSize := s.client.Config.AudioFrameSize()

	if frameSize != s.sourceFrameSize {
		s.deviceSource.CaptureCloseDevice()
		s.sourceFrameSize = frameSize
		s.deviceSource = openal.CaptureOpenDevice(*inputDevice, gumble.AudioSampleRate, openal.FormatMono16, uint32(s.sourceFrameSize))
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	stop := s.sourceStop

	outgoing := s.client.AudioOutgoing()
	defer close(outgoing)

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			buff := s.deviceSource.CaptureSamples(uint32(frameSize))
			if len(buff) != frameSize*2 {
				continue
			}
			int16Buffer := make([]int16, frameSize)
			for i := range int16Buffer {
				int16Buffer[i] = int16(binary.LittleEndian.Uint16(buff[i*2 : (i+1)*2]))
				//				int16Buffer[i] = int16(float32(binary.LittleEndian.Uint16(buff[i*2 : (i+1)*2]))*s.micVolume)
			}
			outgoing <- gumble.AudioBuffer(int16Buffer)
		}
	}
}
