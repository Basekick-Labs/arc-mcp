package main

import "testing"

func TestValidateArcURL(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		token    string
		insecure bool
		wantErr  bool
	}{
		{"https remote with token ok", "https://arc.example.com", "abc", false, false},
		{"http localhost with token ok", "http://localhost:8000", "abc", false, false},
		{"http 127.0.0.1 with token ok", "http://127.0.0.1:8000", "abc", false, false},
		{"http ::1 with token ok", "http://[::1]:8000", "abc", false, false},
		{"http remote with token rejected", "http://arc.example.com", "abc", false, true},
		{"http remote without token ok", "http://arc.example.com", "", false, false},
		{"http remote with token and insecure ok", "http://arc.example.com", "abc", true, false},
		{"empty url rejected", "", "abc", false, true},
		{"bad scheme rejected", "ftp://arc.example.com", "abc", false, true},
		{"missing host rejected", "https://", "abc", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateArcURL(tt.url, tt.token, tt.insecure)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateArcURL(%q, token=%q, insecure=%v) err = %v, wantErr %v", tt.url, tt.token, tt.insecure, err, tt.wantErr)
			}
		})
	}
}
