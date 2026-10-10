package wa

import "testing"

func TestExtractDirectPathFromURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "keeps the query string, which carries the oh/oe auth tokens",
			url:  "https://mmg.whatsapp.net/v/t62.7161-24/1234_n.enc?ccb=11-4&oh=abcd1234&oe=65F0A1B2&_nc_sid=abc123",
			want: "/v/t62.7161-24/1234_n.enc?ccb=11-4&oh=abcd1234&oe=65F0A1B2&_nc_sid=abc123",
		},
		{
			name: "url with no query string",
			url:  "https://mmg.whatsapp.net/v/t62.7161-24/1234_n.enc",
			want: "/v/t62.7161-24/1234_n.enc",
		},
		{
			name: "url with no \".net/\" falls back to the input unchanged",
			url:  "not-a-whatsapp-url",
			want: "not-a-whatsapp-url",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractDirectPathFromURL(tt.url); got != tt.want {
				t.Errorf("extractDirectPathFromURL(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}
