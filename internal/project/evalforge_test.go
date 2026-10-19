package project

import "testing"

// DESIGN 5.29 rule 17: a clone URL is https, except http to exactly the host that
// EVAL_FORGE_URL names.
func TestValidateRepoURLEvalForge(t *testing.T) {
	cases := []struct {
		name, env, url string
		ok             bool
	}{
		{"http without the setting", "", "http://forgefake:8090/acme/app.git", false},
		{"http to the named host", "http://forgefake:8090", "http://forgefake:8090/acme/app.git", true},
		{"the host compares without case", "http://ForgeFake:8090", "http://FORGEFAKE:8090/acme/app.git", true},
		{"http to another host", "http://forgefake:8090", "http://other:8090/acme/app.git", false},
		{"http to another port", "http://forgefake:8090", "http://forgefake:9999/acme/app.git", false},
		{"https stays fine", "http://forgefake:8090", "https://gitlab.example.com/acme/app.git", true},
		{"https to the named host is fine", "http://forgefake:8090", "https://forgefake:8090/acme/app.git", true},
		{"an https setting allows no http host", "https://forgefake:8090", "http://forgefake:8090/acme/app.git", false},
		{"credentials stay refused", "http://forgefake:8090", "http://u:p@forgefake:8090/acme/app.git", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(EvalForgeEnv, c.env)
			_, err := ValidateRepoURL(c.url)
			if (err == nil) != c.ok {
				t.Fatalf("ValidateRepoURL(%q) with %q: %v", c.url, c.env, err)
			}
		})
	}
	t.Setenv(EvalForgeEnv, "http://forgefake:8090")
	cfg, err := Parse("http://forgefake:8090/acme/app.git", "tok", "main", "gitlab", false)
	if err != nil || cfg.APIBase() != "http://forgefake:8090/api/v4/projects/acme%2Fapp" {
		t.Fatalf("Parse: %+v %v", cfg, err)
	}
}
