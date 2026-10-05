package compose

import (
	"strings"
	"testing"
)

const ws = "/var/lib/armageddon/workspaces/abcd1234/tree"

func TestRejects(t *testing.T) {
	cases := map[string]string{
		"privileged":   "services:\n  a:\n    image: x\n    privileged: true\n",
		"cap_add":      "services:\n  a:\n    image: x\n    cap_add: [SYS_ADMIN]\n",
		"host network": "services:\n  a:\n    image: x\n    network_mode: host\n",
		"host pid":     "services:\n  a:\n    image: x\n    pid: host\n",
		"host ipc":     "services:\n  a:\n    image: x\n    ipc: host\n",
		"devices":      "services:\n  a:\n    image: x\n    devices: [\"/dev/kmsg:/dev/kmsg\"]\n",
		"bind outside": "services:\n  a:\n    image: x\n    volumes: [\"/etc:/host-etc\"]\n",
		"bind dotdot":  "services:\n  a:\n    image: x\n    volumes: [\"../../keys:/k\"]\n",
		"socket short": "services:\n  a:\n    image: x\n    volumes: [\"/var/run/docker.sock:/var/run/docker.sock\"]\n",
		"socket long":  "services:\n  a:\n    image: x\n    volumes:\n      - type: bind\n        source: /run/docker.sock\n        target: /s\n",
		"long outside": "services:\n  a:\n    image: x\n    volumes:\n      - type: bind\n        source: /root\n        target: /r\n",
		"unparseable":  "services: [\n",
	}
	for name, y := range cases {
		if errs := Validate([]byte(y), ws); len(errs) == 0 {
			t.Errorf("%s: accepted, want rejection", name)
		}
	}
}

func TestAccepts(t *testing.T) {
	y := `services:
  web:
    image: node:20
    network_mode: bridge
    volumes:
      - ./src:/app
      - data:/var/lib/data
      - type: bind
        source: ./cfg
        target: /cfg
  db:
    image: postgres:16
volumes:
  data: {}
`
	if errs := Validate([]byte(y), ws); len(errs) != 0 {
		t.Fatalf("rejected a safe file: %s", strings.Join(errs, "; "))
	}
}
