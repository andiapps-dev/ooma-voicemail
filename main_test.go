package main

import (
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

// row builds a single <tr data-id="..."> as goquery would see it on the real
// voicemail page, so extractCaller/extractTimestamp/extractAudioURL can be
// exercised without a live Ooma account.
func row(t *testing.T, html string) *goquery.Selection {
	t.Helper()
	doc, err := goquery.NewDocumentFromReader(strings.NewReader("<table><tbody>" + html + "</tbody></table>"))
	if err != nil {
		t.Fatalf("parsing fixture html: %v", err)
	}
	sel := doc.Find("tr[data-id]")
	if sel.Length() != 1 {
		t.Fatalf("fixture must contain exactly one tr[data-id], got %d", sel.Length())
	}
	return sel
}

func TestExtractCallerPrefersDataNumber(t *testing.T) {
	s := row(t, `<tr data-id="123">
		<td><span class="v_cont_name" data-number="+15551234567" data-name="Jane Doe">Jane Doe</span></td>
	</tr>`)
	if got := extractCaller(s); got != "+15551234567" {
		t.Errorf("extractCaller() = %q, want %q", got, "+15551234567")
	}
}

func TestExtractCallerFallsBackToDataName(t *testing.T) {
	s := row(t, `<tr data-id="123">
		<td><span class="v_cont_name" data-name="Jane Doe">Jane Doe</span></td>
	</tr>`)
	if got := extractCaller(s); got != "Jane Doe" {
		t.Errorf("extractCaller() = %q, want %q", got, "Jane Doe")
	}
}

func TestExtractCallerFallsBackToText(t *testing.T) {
	s := row(t, `<tr data-id="123">
		<td><span class="v_cont_name">  Unknown Caller  </span></td>
	</tr>`)
	if got := extractCaller(s); got != "Unknown Caller" {
		t.Errorf("extractCaller() = %q, want %q", got, "Unknown Caller")
	}
}

func TestExtractTimestampParsesDataDate(t *testing.T) {
	s := row(t, `<tr data-id="123">
		<td data-date="11_26_2024_09_20_PM">Nov 26</td>
	</tr>`)
	want := "2024-11-26 21:20"
	if got := extractTimestamp(s); got != want {
		t.Errorf("extractTimestamp() = %q, want %q", got, want)
	}
}

func TestExtractTimestampFallsBackToRawOnParseFailure(t *testing.T) {
	s := row(t, `<tr data-id="123">
		<td data-date="not-a-date">Nov 26</td>
	</tr>`)
	if got := extractTimestamp(s); got != "not-a-date" {
		t.Errorf("extractTimestamp() = %q, want raw value preserved", got)
	}
}

func TestExtractTimestampFallsBackToVisibleColumn(t *testing.T) {
	s := row(t, `<tr data-id="123">
		<td>a</td><td>b</td><td>c</td><td>d</td><td> Nov 26, 2024 </td>
	</tr>`)
	if got := extractTimestamp(s); got != "Nov 26, 2024" {
		t.Errorf("extractTimestamp() = %q, want %q", got, "Nov 26, 2024")
	}
}

func TestExtractAudioURLFromAudioTag(t *testing.T) {
	s := row(t, `<tr data-id="123"><td><audio src="/vm/123.mp3"></audio></td></tr>`)
	if got := extractAudioURL(s); got != "/vm/123.mp3" {
		t.Errorf("extractAudioURL() = %q, want %q", got, "/vm/123.mp3")
	}
}

func TestExtractAudioURLFromDownloadIcon(t *testing.T) {
	s := row(t, `<tr data-id="123"><td><a href="/dl/123"><i class="fa fa-download"></i></a></td></tr>`)
	if got := extractAudioURL(s); got != "/dl/123" {
		t.Errorf("extractAudioURL() = %q, want %q", got, "/dl/123")
	}
}

func TestExtractAudioURLFromMp3Suffix(t *testing.T) {
	s := row(t, `<tr data-id="123"><td><a href="https://cdn.example.com/123.mp3">download</a></td></tr>`)
	if got := extractAudioURL(s); got != "https://cdn.example.com/123.mp3" {
		t.Errorf("extractAudioURL() = %q, want %q", got, "https://cdn.example.com/123.mp3")
	}
}

func TestExtractAudioURLFromDataAttr(t *testing.T) {
	s := row(t, `<tr data-id="123"><td><i class="fa-download" data-url="/dl/123"></i></td></tr>`)
	if got := extractAudioURL(s); got != "/dl/123" {
		t.Errorf("extractAudioURL() = %q, want %q", got, "/dl/123")
	}
}

func TestExtractAudioURLReturnsEmptyWhenNotFound(t *testing.T) {
	s := row(t, `<tr data-id="123"><td>no link here</td></tr>`)
	if got := extractAudioURL(s); got != "" {
		t.Errorf("extractAudioURL() = %q, want empty string", got)
	}
}

func TestOomaBaseURL(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"https://my.ooma.com/login", "https://my.ooma.com", false},
		{"https://my.ooma.com/login?x=1", "https://my.ooma.com", false},
		{"not a url", "", true},
		{"/just/a/path", "", true},
	}
	for _, tt := range tests {
		got, err := oomaBaseURL(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("oomaBaseURL(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("oomaBaseURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
