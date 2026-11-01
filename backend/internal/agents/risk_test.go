package agents

import "testing"

func TestShellRisk(t *testing.T) {
	risky := []string{
		"rm -rf build", "cd x && rm file.txt", "git push origin main", "git reset --hard HEAD~1",
		"ssh root@1.2.3.4 uptime", "scp a b:", "curl -X POST https://x", "curl -d 'a=1' https://x",
		"npm publish", "docker run nginx", "kubectl apply -f a.yaml", "find . -name '*.pyc' -delete",
	}
	safe := []string{
		"python3 monitor.py --help", "pytest -q", "curl -sI https://example.com", "git init && git add -A && git commit -m init",
		"ls -la", "grep -rn password .", "cat README.md", "python3 -m http.server 8000 &",
		"cat > notes.md <<EOF\n- planned: ssh deploy@host uptime\n- rm -rf was not run\nEOF\ncat notes.md",
	}
	// a heredoc must not hide a risky command after it
	if ShellRisk("cat > a.md <<'EOF'\ntext\nEOF\nrm -rf build") == "" {
		t.Error("risky command after heredoc was not detected")
	}
	for _, c := range risky {
		if ShellRisk(c) == "" {
			t.Errorf("expected risky: %q", c)
		}
	}
	for _, c := range safe {
		if r := ShellRisk(c); r != "" {
			t.Errorf("expected safe: %q (got %s)", c, r)
		}
	}
}
