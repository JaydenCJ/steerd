// Wire-format tests for steer/1: framing, size limits, resynchronisation
// after bad input, and directive validation. Everything here is pure —
// no sockets, no goroutines.
package steerd

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func decodeAll(t *testing.T, data string) []Frame {
	t.Helper()
	r := bufio.NewReaderSize(strings.NewReader(data), 64)
	var frames []Frame
	for {
		f, err := DecodeFrame(r)
		if errors.Is(err, io.EOF) {
			return frames
		}
		if err != nil {
			t.Fatalf("DecodeFrame: %v", err)
		}
		frames = append(frames, f)
	}
}

func TestEncodeFrameProducesOneNewlineTerminatedLine(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeFrame(&buf, Frame{Type: "ack", Stage: StageAccepted, Seq: 3}); err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	s := buf.String()
	if !strings.HasSuffix(s, "\n") {
		t.Fatalf("encoded frame must end with newline, got %q", s)
	}
	if strings.Count(s, "\n") != 1 {
		t.Fatalf("encoded frame must be exactly one line, got %q", s)
	}
}

func TestEncodeFrameOmitsEmptyFields(t *testing.T) {
	// The wire stays small and future fields stay optional because every
	// field except type is omitempty.
	var buf bytes.Buffer
	if err := EncodeFrame(&buf, Frame{Type: "ack"}); err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	if got := buf.String(); got != "{\"type\":\"ack\"}\n" {
		t.Fatalf("minimal ack should encode tightly, got %q", got)
	}
}

func TestEncodeDecodeRoundTripPreservesAllFields(t *testing.T) {
	in := Frame{
		Type: "event", Proto: ProtocolVersion, PID: 42, ID: "d7", Op: OpRedirect,
		Message: "skip the appendix", Mode: "append", Reason: "why",
		Stage: StageApplied, Seq: 9, Error: "", Agent: "worker",
		State: StatePaused, Step: 12, Task: "t", Note: "n", Pending: 2, Event: "redirected",
	}
	var buf bytes.Buffer
	if err := EncodeFrame(&buf, in); err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	out, err := DecodeFrame(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if out != in {
		t.Fatalf("round trip mismatch:\n in %+v\nout %+v", in, out)
	}
}

func TestEncodeFrameRefusesOversizedFrameWithoutWriting(t *testing.T) {
	var buf bytes.Buffer
	err := EncodeFrame(&buf, Frame{Type: "directive", Op: OpRedirect, ID: "d1",
		Message: strings.Repeat("a", MaxFrameSize)})
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("want ErrFrameTooLarge, got %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("nothing may be written for a refused frame, got %d bytes", buf.Len())
	}
}

func TestDecodeFrameReadsSeveralFramesFromOneStream(t *testing.T) {
	frames := decodeAll(t, "{\"type\":\"hello\"}\n{\"type\":\"ack\",\"seq\":1}\n{\"type\":\"state\",\"step\":5}\n")
	if len(frames) != 3 {
		t.Fatalf("want 3 frames, got %d", len(frames))
	}
	if frames[2].Step != 5 {
		t.Fatalf("third frame step = %d, want 5", frames[2].Step)
	}
}

func TestDecodeFrameHandlesLinesLongerThanTheReaderBuffer(t *testing.T) {
	// The bufio buffer in decodeAll is only 64 bytes; a legitimate 1 KiB
	// frame must still decode via the ErrBufferFull continuation path.
	msg := strings.Repeat("x", 1024)
	frames := decodeAll(t, "{\"type\":\"directive\",\"op\":\"redirect\",\"id\":\"d1\",\"message\":\""+msg+"\"}\n")
	if len(frames) != 1 || frames[0].Message != msg {
		t.Fatalf("long frame did not survive decoding")
	}
}

func TestDecodeFrameRejectsMalformedJSONButKeepsStreamAlive(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader("this is not json\n{\"type\":\"ack\"}\n"), 64)
	_, err := DecodeFrame(r)
	if !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("want ErrMalformedFrame, got %v", err)
	}
	f, err := DecodeFrame(r)
	if err != nil || f.Type != "ack" {
		t.Fatalf("stream must stay usable after malformed input, got %+v, %v", f, err)
	}
}

func TestDecodeFrameRejectsFrameWithoutType(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("{\"op\":\"pause\"}\n"))
	_, err := DecodeFrame(r)
	if !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("want ErrMalformedFrame for missing type, got %v", err)
	}
}

func TestDecodeFrameRejectsOversizedLineAndResynchronises(t *testing.T) {
	// One hostile 100 KiB line must not poison the connection: the reader
	// reports ErrFrameTooLarge and the next frame decodes normally.
	huge := strings.Repeat("z", 100*1024)
	r := bufio.NewReaderSize(strings.NewReader(huge+"\n{\"type\":\"state\"}\n"), 64)
	_, err := DecodeFrame(r)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("want ErrFrameTooLarge, got %v", err)
	}
	f, err := DecodeFrame(r)
	if err != nil || f.Type != "state" {
		t.Fatalf("stream must resynchronise after oversized line, got %+v, %v", f, err)
	}
}

func TestDecodeFrameAcceptsFrameExactlyAtTheSizeLimit(t *testing.T) {
	// Boundary check: a frame of exactly MaxFrameSize bytes (newline
	// included) is legal; one byte more is not.
	prefix, suffix := "{\"type\":\"ack\",\"error\":\"", "\"}"
	pad := MaxFrameSize - 1 - len(prefix) - len(suffix) // -1 for the newline
	line := prefix + strings.Repeat("e", pad) + suffix + "\n"
	if len(line) != MaxFrameSize {
		t.Fatalf("test bug: line is %d bytes, want %d", len(line), MaxFrameSize)
	}
	f, err := DecodeFrame(bufio.NewReaderSize(strings.NewReader(line), 4096))
	if err != nil || len(f.Error) != pad {
		t.Fatalf("limit-sized frame should decode, got err %v", err)
	}
	_, err = DecodeFrame(bufio.NewReaderSize(strings.NewReader("{"+line), 4096))
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("limit+1 frame should be refused, got %v", err)
	}
}

func TestDecodeFrameReturnsEOFOnCleanEndOfStream(t *testing.T) {
	_, err := DecodeFrame(bufio.NewReader(strings.NewReader("")))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("want io.EOF, got %v", err)
	}
}

func TestDecodeFrameReportsTruncatedFinalFrame(t *testing.T) {
	// A frame missing its newline means the peer died mid-write; that must
	// not be silently decoded as if it were complete.
	_, err := DecodeFrame(bufio.NewReader(strings.NewReader("{\"type\":\"ack\"}")))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("want io.ErrUnexpectedEOF, got %v", err)
	}
}

func TestValidateDirectiveAcceptsEveryWellFormedOp(t *testing.T) {
	ok := []Frame{
		{Type: "directive", ID: "d1", Op: OpPause, Reason: "checking on you"},
		{Type: "directive", ID: "d2", Op: OpResume},
		{Type: "directive", ID: "d3", Op: OpRedirect, Message: "focus on cmd/"},
		{Type: "directive", ID: "d4", Op: OpRedirect, Message: "m", Mode: "replace"},
		{Type: "directive", ID: "d5", Op: OpCancel, Reason: "wrong branch"},
		{Type: "directive", Op: OpStatus},    // status needs no id
		{Type: "directive", Op: OpSubscribe}, // subscribe needs no id
	}
	for _, f := range ok {
		if err := f.validateDirective(); err != nil {
			t.Errorf("op %q should validate, got %v", f.Op, err)
		}
	}
}

func TestValidateDirectiveRejectsUnknownOp(t *testing.T) {
	f := Frame{Type: "directive", ID: "d1", Op: "reboot"}
	if err := f.validateDirective(); err == nil || !strings.Contains(err.Error(), "unknown op") {
		t.Fatalf("want unknown-op error, got %v", err)
	}
}

func TestValidateDirectiveRejectsMissingOpAndMissingID(t *testing.T) {
	if err := (Frame{Type: "directive", ID: "d1"}).validateDirective(); err == nil ||
		!strings.Contains(err.Error(), "missing an op") {
		t.Fatalf("want missing-op error, got %v", err)
	}
	if err := (Frame{Type: "directive", Op: OpPause}).validateDirective(); err == nil ||
		!strings.Contains(err.Error(), "requires an id") {
		t.Fatalf("want missing-id error, got %v", err)
	}
}

func TestValidateDirectiveRejectsRedirectWithoutMessageOrWithBadMode(t *testing.T) {
	if err := (Frame{Type: "directive", ID: "d1", Op: OpRedirect}).validateDirective(); err == nil ||
		!strings.Contains(err.Error(), "requires a non-empty message") {
		t.Fatalf("want missing-message error, got %v", err)
	}
	f := Frame{Type: "directive", ID: "d1", Op: OpRedirect, Message: "m", Mode: "prepend"}
	if err := f.validateDirective(); err == nil || !strings.Contains(err.Error(), "unknown redirect mode") {
		t.Fatalf("want bad-mode error, got %v", err)
	}
}

func TestValidateDirectiveRejectsPayloadOnPayloadlessOps(t *testing.T) {
	// A pause carrying a message is almost certainly a controller bug
	// (meant redirect); refusing it loudly beats ignoring the payload.
	f := Frame{Type: "directive", ID: "d1", Op: OpPause, Message: "oops"}
	if err := f.validateDirective(); err == nil || !strings.Contains(err.Error(), "takes no message") {
		t.Fatalf("want no-payload error, got %v", err)
	}
}

func TestValidateDirectiveRejectsWrongFrameType(t *testing.T) {
	f := Frame{Type: "ack", ID: "d1", Op: OpPause}
	if err := f.validateDirective(); err == nil || !strings.Contains(err.Error(), "unexpected frame type") {
		t.Fatalf("want frame-type error, got %v", err)
	}
}
