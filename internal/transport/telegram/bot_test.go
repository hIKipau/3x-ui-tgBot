package telegram

import "testing"

func TestInboundName(t *testing.T) {
	tests := []struct {
		name  string
		link  string
		email string
		want  string
	}{
		{
			name:  "inbound name without generated client suffix",
			link:  "vless://id@example.com:443#%F0%9F%87%A7%F0%9F%87%AC%20LTE-%40hikipau",
			email: "@hikipau",
			want:  "🇧🇬 LTE",
		},
		{name: "exact inbound name", link: "vless://id@example.com:443#Home%20VPN", want: "Home VPN"},
		{name: "fallback", link: "not a URL", want: "Inbound 3"},
	}

	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := inboundName(tt.link, tt.email, index); got != tt.want {
				t.Fatalf("inboundName() = %q, want %q", got, tt.want)
			}
		})
	}
}
