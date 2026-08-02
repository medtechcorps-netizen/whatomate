package handlers

import "testing"

func TestValidateCustomActionRedirectURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "https", url: "https://crm.example.com/contact/{{contact.id}}"},
		{name: "app relative", url: "/contacts/{{contact.id}}?tab=activity"},
		{name: "http", url: "http://crm.example.com", wantErr: true},
		{name: "javascript", url: "javascript:alert(1)", wantErr: true},
		{name: "data", url: "data:text/html,hello", wantErr: true},
		{name: "protocol relative", url: "//evil.example.com", wantErr: true},
		{name: "userinfo", url: "https://user:pass@crm.example.com", wantErr: true},
		{name: "encoded CRLF", url: "https://crm.example.com/%0d%0aLocation:%20https://evil.example.com", wantErr: true},
		{name: "backslash", url: "https:\\evil.example.com", wantErr: true},
		{name: "bare relative", url: "contacts/123", wantErr: true},
		{name: "empty", url: "", wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateCustomActionRedirectURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateCustomActionRedirectURL(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
			}
		})
	}
}

func TestStoreCustomActionRedirectRejectsUnsafeTarget(t *testing.T) {
	t.Parallel()

	if _, err := storeCustomActionRedirect("javascript:alert(document.domain)"); err == nil {
		t.Fatal("storeCustomActionRedirect accepted an executable URL")
	}
}
