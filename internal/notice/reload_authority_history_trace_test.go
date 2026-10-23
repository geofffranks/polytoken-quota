package notice

import (
	"testing"
)

func TestNoticeAuthorityHistorySemanticTraceRepeatsForSameSeed(t *testing.T) {
	first := noticeAuthorityHistoryChoices(8675309, 28)
	repeated := noticeAuthorityHistoryChoices(8675309, 28)
	if got, want := noticeAuthorityHistoryTrace(first), noticeAuthorityHistoryTrace(repeated); got != want {
		t.Fatalf("same seed produced different semantic traces: %x != %x", got, want)
	}
	if got, other := noticeAuthorityHistoryTrace(first), noticeAuthorityHistoryTrace(noticeAuthorityHistoryChoices(8675310, 28)); got == other {
		t.Fatalf("different seeds unexpectedly produced the same semantic trace: %x", got)
	}
	if len(first) != 28 {
		t.Fatalf("trace choices=%d, want 28", len(first))
	}
}
