package connect_test

import (
	"testing"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/gitprovider"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/gitprovider/connect"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/gitea"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/gitlab"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

func TestEachProviderOpensItsClient(t *testing.T) {
	o := gitprovider.Options{}
	repo, err := source.ParseRepoName("acme/shop")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[store.GitProvider]string{
		store.GitProviderGitHub: "https://github.com/acme/shop.git",
		store.GitProviderGitLab: "https://git.example.com/acme/shop.git",
		store.GitProviderGitea:  "https://git.example.com/acme/shop.git",
	}
	for provider, clone := range cases {
		base := "https://git.example.com"
		if provider == store.GitProviderGitHub {
			base = gitprovider.GitHubAPI
		}
		c, err := connect.Open(o, provider, base, "t")
		if err != nil {
			t.Fatalf("%s: %v", provider, err)
		}
		if got := c.CloneURL(repo); got != clone {
			t.Errorf("%s: %s", provider, got)
		}
		switch provider {
		case store.GitProviderGitLab:
			if _, ok := c.(*gitlab.Client); !ok {
				t.Errorf("gitlab: %T", c)
			}
		case store.GitProviderGitea:
			if _, ok := c.(*gitea.Client); !ok {
				t.Errorf("gitea: %T", c)
			}
		case store.GitProviderGitHub:
		}
	}
	if _, err := connect.Open(o, "bitbucket", "https://x", "t"); err == nil {
		t.Error("an unknown provider")
	}
}

func TestSealedSecretsOpenForTheirConnectionOnly(t *testing.T) {
	var key [32]byte
	key[0] = 7
	ring := keyring.FromKeys(map[uint32][32]byte{1: key})
	org := ids.New[ids.Org]()
	c := store.GitConnection{ID: ids.New[ids.GitConnection](), Org: org, Provider: store.GitProviderGitLab, Name: "acme"}
	var err error
	if c.Token, err = ring.Seal(keyring.GitTokenIdentity(org, c.ID), []byte("glpat-x")); err != nil {
		t.Fatal(err)
	}
	if c.WebhookSecret, err = ring.Seal(keyring.GitWebhookIdentity(org, c.ID), []byte("hook")); err != nil {
		t.Fatal(err)
	}
	if token, err := connect.Token(ring, c); err != nil || token != "glpat-x" {
		t.Errorf("token %q %v", token, err)
	}
	if secret, err := connect.WebhookSecret(ring, c); err != nil || string(secret) != "hook" {
		t.Errorf("secret %q %v", secret, err)
	}
	// The token sealed for one connection does not open as another's, nor
	// as a webhook secret.
	moved := c
	moved.ID = ids.New[ids.GitConnection]()
	if _, err := connect.Token(ring, moved); err == nil {
		t.Error("a token opened for another connection")
	}
	swapped := c
	swapped.WebhookSecret = c.Token
	if _, err := connect.WebhookSecret(ring, swapped); err == nil {
		t.Error("a token opened as a webhook secret")
	}
	if client, err := connect.Client(gitprovider.Options{}, ring, c); err != nil || client == nil {
		t.Errorf("client %v", err)
	}
}
