package safetext

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

// --- invalid UTF-8 ---

// TestNormalizeInvalidUTF8ReplacedDeterministically asserts the exact
// output bytes for an invalid UTF-8 input, not merely "doesn't crash": the
// same invalid input must always produce the same output, and that output
// must be the standard U+FFFD replacement encoding.
func TestNormalizeInvalidUTF8ReplacedDeterministically(t *testing.T) {
	t.Parallel()
	// 0xFF is never valid anywhere in UTF-8 (not a valid lead or
	// continuation byte).
	input := []byte("ab\xFFcd")
	want := "ab" + string(utf8.RuneError) + "cd"

	var n1, n2 Normalizer
	got1 := string(n1.Normalize(input))
	got2 := string(n2.Normalize(input))

	if got1 != want {
		t.Errorf("Normalize() = %q, want %q", got1, want)
	}
	if got1 != got2 {
		t.Errorf("Normalize() is not deterministic: %q != %q", got1, got2)
	}
}

func TestNormalizeInvalidUTF8ContinuationByteAlone(t *testing.T) {
	t.Parallel()
	// A lone continuation byte (0x80-0xBF) with no preceding lead byte is
	// always invalid on its own.
	input := []byte{'x', 0x80, 'y'}
	var n Normalizer
	got := string(n.Normalize(input))
	want := "x" + string(utf8.RuneError) + "y"
	if got != want {
		t.Errorf("Normalize() = %q, want %q", got, want)
	}
}

// --- C0/C1 controls except approved whitespace ---

func TestNormalizeStripsC0ControlsExceptApprovedWhitespace(t *testing.T) {
	t.Parallel()
	var n Normalizer
	input := []byte("a\x00b\x01c\x07d") // NUL, SOH, BEL are all C0 controls, none approved
	got := string(n.Normalize(input))
	want := "abcd"
	if got != want {
		t.Errorf("Normalize() = %q, want %q (C0 controls stripped)", got, want)
	}
}

func TestNormalizeKeepsApprovedWhitespace(t *testing.T) {
	t.Parallel()
	var n Normalizer
	input := []byte("a\tb\nc\rd e")
	got := string(n.Normalize(input))
	if got != string(input) {
		t.Errorf("Normalize() = %q, want unchanged %q (space/tab/newline/CR are approved whitespace)", got, input)
	}
}

func TestNormalizeStripsC1ControlCodepoints(t *testing.T) {
	t.Parallel()
	var n Normalizer
	// U+0085 (NEL) and U+009B (CSI as a single C1 codepoint) are C1
	// controls, encoded here as their proper 2-byte UTF-8 forms -- not raw
	// bytes 0x85/0x9B, which would be invalid UTF-8 on their own.
	input := []byte("a" + string(rune(0x85)) + "b" + string(rune(0x9B)) + "c")
	got := string(n.Normalize(input))
	want := "abc"
	if got != want {
		t.Errorf("Normalize() = %q, want %q (C1 controls stripped)", got, want)
	}
}

// --- CSI, OSC, DCS sequences ---

func TestNormalizeRemovesCSISequence(t *testing.T) {
	t.Parallel()
	var n Normalizer
	// CSI: ESC [ 3 1 m (SGR red foreground), then reset ESC [ 0 m.
	input := []byte("red\x1b[31mtext\x1b[0mplain")
	got := string(n.Normalize(input))
	want := "redtextplain"
	if got != want {
		t.Errorf("Normalize() = %q, want %q", got, want)
	}
}

func TestNormalizeRemovesOSCSequenceTerminatedByBEL(t *testing.T) {
	t.Parallel()
	var n Normalizer
	// OSC 0 sets the window title, terminated by BEL.
	input := []byte("before\x1b]0;title text\x07after")
	got := string(n.Normalize(input))
	want := "beforeafter"
	if got != want {
		t.Errorf("Normalize() = %q, want %q", got, want)
	}
}

func TestNormalizeRemovesOSCSequenceTerminatedBySTForm(t *testing.T) {
	t.Parallel()
	var n Normalizer
	// OSC terminated by the string-terminator form: ESC \.
	input := []byte("before\x1b]0;title text\x1b\\after")
	got := string(n.Normalize(input))
	want := "beforeafter"
	if got != want {
		t.Errorf("Normalize() = %q, want %q", got, want)
	}
}

func TestNormalizeRemovesDCSSequence(t *testing.T) {
	t.Parallel()
	var n Normalizer
	// DCS: ESC P ... ESC \.
	input := []byte("before\x1bPsome device control string\x1b\\after")
	got := string(n.Normalize(input))
	want := "beforeafter"
	if got != want {
		t.Errorf("Normalize() = %q, want %q", got, want)
	}
}

// --- terminal sequences split across Normalize call boundaries ---

// TestNormalizeCSISequenceSplitAcrossCalls feeds a CSI sequence's bytes to
// two separate Normalize calls, split at the ESC byte itself, and asserts
// no partial escape byte leaks into either call's output.
func TestNormalizeCSISequenceSplitAcrossCalls(t *testing.T) {
	t.Parallel()
	var n Normalizer
	part1 := n.Normalize([]byte("red\x1b"))  // ends exactly on the ESC byte
	part2 := n.Normalize([]byte("[31mtext")) // sequence introducer + body arrive next call
	got := string(part1) + string(part2)
	want := "redtext"
	if got != want {
		t.Errorf("Normalize() split = %q, want %q", got, want)
	}
	if bytes.ContainsRune(part1, 0x1B) || bytes.ContainsRune(part2, 0x1B) {
		t.Errorf("a raw ESC byte leaked into output: part1=%q part2=%q", part1, part2)
	}
}

// TestNormalizeCSISequenceSplitMidParameters splits a CSI sequence in the
// middle of its parameter bytes, well after the introducer, to prove the
// carry-over state machine (not just pending-byte buffering) survives the
// boundary.
func TestNormalizeCSISequenceSplitMidParameters(t *testing.T) {
	t.Parallel()
	var n Normalizer
	part1 := n.Normalize([]byte("x\x1b[3"))
	part2 := n.Normalize([]byte("1my"))
	got := string(part1) + string(part2)
	want := "xy"
	if got != want {
		t.Errorf("Normalize() split = %q, want %q", got, want)
	}
}

// TestNormalizeOSCSequenceSplitAcrossCalls splits an OSC sequence between
// its introducer and its BEL terminator across two Normalize calls.
func TestNormalizeOSCSequenceSplitAcrossCalls(t *testing.T) {
	t.Parallel()
	var n Normalizer
	part1 := n.Normalize([]byte("a\x1b]0;partial titl"))
	part2 := n.Normalize([]byte("e\x07b"))
	got := string(part1) + string(part2)
	want := "ab"
	if got != want {
		t.Errorf("Normalize() split = %q, want %q", got, want)
	}
}

// TestNormalizeOSCSequenceSTSplitAtTerminator splits an OSC ST-form
// terminator (ESC \) exactly between the ESC and the backslash, the
// hardest boundary case: the sequence isn't known to be complete until the
// very first byte of the next chunk.
func TestNormalizeOSCSequenceSTSplitAtTerminator(t *testing.T) {
	t.Parallel()
	var n Normalizer
	part1 := n.Normalize([]byte("a\x1b]0;title\x1b"))
	part2 := n.Normalize([]byte("\\b"))
	got := string(part1) + string(part2)
	want := "ab"
	if got != want {
		t.Errorf("Normalize() split = %q, want %q", got, want)
	}
}

// TestNormalizeDCSSequenceSplitAcrossCalls mirrors the OSC split test for
// DCS.
func TestNormalizeDCSSequenceSplitAcrossCalls(t *testing.T) {
	t.Parallel()
	var n Normalizer
	part1 := n.Normalize([]byte("a\x1bPdevice contro"))
	part2 := n.Normalize([]byte("l string\x1b\\b"))
	got := string(part1) + string(part2)
	want := "ab"
	if got != want {
		t.Errorf("Normalize() split = %q, want %q", got, want)
	}
}

// TestNormalizeMultiByteRuneSplitAcrossCalls splits a valid multi-byte
// UTF-8 rune's encoding across two Normalize calls and verifies it decodes
// correctly once reassembled, rather than being prematurely replaced with
// U+FFFD by the first call.
func TestNormalizeMultiByteRuneSplitAcrossCalls(t *testing.T) {
	t.Parallel()
	var n Normalizer
	raw := []byte("héllo") // 'é' is 2 bytes: 0xC3 0xA9
	idx := bytes.IndexByte(raw, 0xC3)
	if idx < 0 {
		t.Fatalf("test fixture bug: expected 0xC3 lead byte in %q", raw)
	}
	part1 := n.Normalize(raw[:idx+1]) // ends right after the lead byte
	part2 := n.Normalize(raw[idx+1:])
	got := string(part1) + string(part2)
	if got != "héllo" {
		t.Errorf("Normalize() split multi-byte rune = %q, want %q", got, "héllo")
	}
}

// --- binary detection ---

func TestLooksBinaryNULHeavySample(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte{0, 'a', 0, 'b'}, 20) // 50% NUL, well over threshold
	if !LooksBinary(data) {
		t.Errorf("LooksBinary() = false, want true for NUL-heavy data")
	}
}

func TestLooksBinaryHighEntropySample(t *testing.T) {
	t.Parallel()
	// A deterministic, perfectly-uniform 4096-byte sample: every byte value
	// 0-255 appears exactly 16 times, giving exactly the maximum possible
	// entropy (8 bits/byte) with no reliance on a random source.
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if !LooksBinary(data) {
		t.Errorf("LooksBinary() = false, want true for a uniform high-entropy sample")
	}
}

func TestLooksBinaryOrdinaryTextIsNotBinary(t *testing.T) {
	t.Parallel()
	data := []byte(strings.Repeat("the quick brown fox jumps over the lazy dog\n", 50))
	if LooksBinary(data) {
		t.Errorf("LooksBinary() = true, want false for ordinary repetitive text")
	}
}

func TestLooksBinaryEmptyIsNotBinary(t *testing.T) {
	t.Parallel()
	if LooksBinary(nil) {
		t.Errorf("LooksBinary(nil) = true, want false")
	}
}

func TestLooksBinarySmallSampleBelowEntropyFloorNeedsNUL(t *testing.T) {
	t.Parallel()
	// Below binaryMinSampleBytes, entropy is mathematically incapable of
	// reaching the threshold (bounded by log2(N)); only NUL density can
	// flag a small sample.
	data := []byte("hello")
	if LooksBinary(data) {
		t.Errorf("LooksBinary() = true, want false for a short clean text sample")
	}
}

// --- safe text unchanged ---

func TestNormalizeSafeTextUnchanged(t *testing.T) {
	t.Parallel()
	var n Normalizer
	input := []byte("Ordinary printable ASCII, unicode (héllo, 日本語), tabs\tand newlines\n stay exactly as-is.")
	got := n.Normalize(input)
	if !bytes.Equal(got, input) {
		t.Errorf("Normalize() = %q, want unchanged %q", got, input)
	}
}

// --- capped truncation never splits a replacement sequence ---

func TestTruncateDoesNotSplitReplacementCharacter(t *testing.T) {
	t.Parallel()
	var n Normalizer
	// Build normalized text whose replacement character(s) would land
	// exactly at several candidate cut points, and verify every cut point
	// from 0 to len(text) yields valid UTF-8 with no half replacement char.
	text := n.Normalize([]byte("abc\xFFdef\xFFghi"))
	for limit := 0; limit <= len(text); limit++ {
		got := Truncate(text, limit)
		if !utf8.Valid(got) {
			t.Fatalf("Truncate(text, %d) = %q, not valid UTF-8", limit, got)
		}
		if len(got) > limit {
			t.Fatalf("Truncate(text, %d) returned %d bytes, want <= %d", limit, len(got), limit)
		}
	}
}

func TestTruncateNonPositiveLimitReturnsUnchanged(t *testing.T) {
	t.Parallel()
	data := []byte("hello")
	if got := Truncate(data, -1); !bytes.Equal(got, data) {
		t.Errorf("Truncate(data, -1) = %q, want unchanged %q", got, data)
	}
}

func TestTruncateZeroLimitReturnsEmpty(t *testing.T) {
	t.Parallel()
	got := Truncate([]byte("hello"), 0)
	if len(got) != 0 {
		t.Errorf("Truncate(data, 0) = %q, want empty", got)
	}
}

func TestTruncateUnderLimitReturnsUnchanged(t *testing.T) {
	t.Parallel()
	data := []byte("hi")
	got := Truncate(data, 100)
	if !bytes.Equal(got, data) {
		t.Errorf("Truncate() = %q, want unchanged %q", got, data)
	}
}
