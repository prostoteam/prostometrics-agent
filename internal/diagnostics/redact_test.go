package diagnostics

import "testing"

func TestRedactStructuredCredentials(t *testing.T) {
	for _, tt := range []struct {
		name, input, want string
	}{
		{"json authorization", `{"Authorization":"Bearer private value","status":500}`, `{"Authorization":"[redacted]","status":500}`},
		{"json password", `{"password":"correct horse, battery; staple","status":500}`, `{"password":"[redacted]","status":500}`},
		{"escaped quotes", `{"password":"before\" middle\\ after","status":500}`, `{"password":"[redacted]","status":500}`},
		{"single quotes", `password='private, spaced; value' status=failed`, `password='[redacted]' status=failed`},
		{"escaped single quote", `password='private\' value' status=failed`, `password='[redacted]' status=failed`},
		{"unterminated quote", `password="private partial value`, `password="[redacted]`},
		{"cookie header", "Cookie: tracking=public; session=private; csrf=other\nstatus=failed", "Cookie: [redacted]\nstatus=failed"},
		{"json cookies", `{"Cookie":"tracking=public; session=private","status":500}`, `{"Cookie":"[redacted]","status":500}`},
		{"set cookie", "Set-Cookie: session=private; HttpOnly; Path=/\r\nstatus=500", "Set-Cookie: [redacted]\r\nstatus=500"},
		{"authorization header", "Authorization: Bearer private value\nstatus=500", "Authorization: [redacted]\nstatus=500"},
		{"proxy authorization", "Proxy-Authorization: Basic private\nstatus=500", "Proxy-Authorization: [redacted]\nstatus=500"},
		{"multiple fields", `password=private token="another secret" status=failed`, `password=[redacted] token="[redacted]" status=failed`},
		{"env field", `DATABASE_PASSWORD="private value"`, `DATABASE_PASSWORD="[redacted]"`},
		{"url", `connect mongodb://name:private@host/db failed`, `connect mongodb://[redacted]@host/db failed`},
		{"pem", "before\n-----BEGIN PRIVATE KEY-----\nprivate\n-----END PRIVATE KEY-----\nafter", "before\n[redacted]\nafter"},
		{"token prefix", `token ghp_12345678901234567890 failed`, `token [redacted] failed`},
		{"ordinary log", `nginx.service status=failed exit=1`, `nginx.service status=failed exit=1`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := Redact(tt.input)
			if got != tt.want {
				t.Fatalf("redaction = %q, want %q", got, tt.want)
			}
			if twice := Redact(got); twice != got {
				t.Fatalf("second redaction changed result: %q", twice)
			}
		})
	}
}
