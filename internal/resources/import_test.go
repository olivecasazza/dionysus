package resources

import (
	"strings"
	"testing"

	gamesv1alpha1 "github.com/olivecasazza/dionysus/api/v1alpha1"
)

// An import writes attacker-controlled bytes into a game volume, so the
// filename guard is the load-bearing check. Anything that escapes the
// destination directory is a container-breakout-adjacent bug.
func TestSanitizeFilenameRejectsTraversal(t *testing.T) {
	bad := []string{
		"../../etc/passwd",
		"..",
		".",
		"",
		"/",
		"/absolute.pak",
		"sub/dir/mod.pak",
		`..\..\windows\system32\evil.pak`,
		"-rf.pak",
		"nul\x00.pak",
		"line\nbreak.pak",
		strings.Repeat("a", 129) + ".pak",
	}
	for _, in := range bad {
		if got, err := SanitizeFilename(in); err == nil {
			t.Errorf("SanitizeFilename(%q) = %q, want error", in, got)
		}
	}
}

func TestSanitizeFilenameKeepsPlainNames(t *testing.T) {
	cases := map[string]string{
		"cool_mod.pak":      "cool_mod.pak",
		" spaced.pak ":      "spaced.pak",
		"world.tar.gz":      "world.tar.gz",
		"Mod.With.Dots.pak": "Mod.With.Dots.pak",
	}
	for in, want := range cases {
		got, err := SanitizeFilename(in)
		if err != nil {
			t.Fatalf("SanitizeFilename(%q) unexpected error: %v", in, err)
		}
		if got != want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

// Extensions are an allowlist because a .sh or .so landing in a game volume
// is an execution primitive, not a mod.
func TestValidateImportExtensionAllowlist(t *testing.T) {
	shouldFail := []struct {
		kind ImportKind
		file string
	}{
		{ImportMod, "payload.sh"},
		{ImportMod, "payload.so"},
		{ImportMod, "world.zip"},
		{ImportMod, "noext"},
		{ImportWorld, "mod.pak"},
		{ImportWorld, "run.sh"},
		{"bogus", "x.pak"},
	}
	for _, tc := range shouldFail {
		if err := ValidateImport(tc.kind, tc.file); err == nil {
			t.Errorf("ValidateImport(%q, %q) = nil, want error", tc.kind, tc.file)
		}
	}

	shouldPass := []struct {
		kind ImportKind
		file string
	}{
		{ImportMod, "cool.pak"},
		{ImportMod, "data.ucas"},
		{ImportWorld, "save.zip"},
		{ImportWorld, "save.sav"},
		{ImportWorld, "SAVE.ZIP"}, // case-insensitive
	}
	for _, tc := range shouldPass {
		if err := ValidateImport(tc.kind, tc.file); err != nil {
			t.Errorf("ValidateImport(%q, %q) = %v, want nil", tc.kind, tc.file, err)
		}
	}
}

func palworldGame() *gamesv1alpha1.HostedGame {
	g := &gamesv1alpha1.HostedGame{}
	g.Name = "palworld"
	g.Namespace = "apps"
	g.Spec.Image = "thijsvanloef/palworld-server-docker:v2.7.1"
	g.Spec.Volumes = []gamesv1alpha1.GameVolume{{
		Name:          "data",
		MountPath:     "/palworld",
		ExistingClaim: "palworld-data",
	}}
	return g
}

func TestImportJobRejectsInsecureAndTraversal(t *testing.T) {
	g := palworldGame()

	if _, err := ImportJob(g, ImportMod, "http://evil/mod.pak", "mod.pak", ""); err == nil {
		t.Error("plain http source accepted, want rejection")
	}
	if _, err := ImportJob(g, ImportMod, "https://x/../../evil.pak", "../../evil.pak", ""); err == nil {
		t.Error("traversal filename accepted, want rejection")
	}
	if _, err := ImportJob(g, ImportMod, "https://x/e.sh", "e.sh", ""); err == nil {
		t.Error("non-allowlisted extension accepted, want rejection")
	}
}

func TestImportJobTargetsModDirAndReusesClaim(t *testing.T) {
	g := palworldGame()
	job, err := ImportJob(g, ImportMod, "https://cdn.discordapp.com/a/cool.pak", "cool.pak", "seir")
	if err != nil {
		t.Fatalf("ImportJob: %v", err)
	}

	// Must reuse the live PVC, never provision a new one.
	vols := job.Spec.Template.Spec.Volumes
	if len(vols) != 1 || vols[0].PersistentVolumeClaim == nil ||
		vols[0].PersistentVolumeClaim.ClaimName != "palworld-data" {
		t.Fatalf("job does not mount palworld-data: %+v", vols)
	}

	// RWO co-location: must be pinned to the running game's node.
	if job.Spec.Template.Spec.NodeName != "seir" {
		t.Errorf("NodeName = %q, want seir", job.Spec.Template.Spec.NodeName)
	}

	// Never silently retry a partial write.
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("BackoffLimit = %v, want 0", job.Spec.BackoffLimit)
	}

	script := strings.Join(job.Spec.Template.Spec.Containers[0].Command, " ")
	if !strings.Contains(script, "/palworld/Pal/Content/Paks/~mods") {
		t.Errorf("script does not target the palworld mod dir:\n%s", script)
	}
	// Download-then-rename, so a truncated transfer is never seen as a mod.
	if !strings.Contains(script, ".partial") || !strings.Contains(script, "mv -f") {
		t.Errorf("script lacks atomic staging:\n%s", script)
	}
}

func TestImportDestDirUnknownGameIsRejected(t *testing.T) {
	g := palworldGame()
	g.Spec.Image = "itzg/minecraft-server:java21"
	if _, err := ImportDestDir(g, ImportMod); err == nil {
		t.Error("unknown game accepted for mod import; want explicit rejection rather than guessing a path")
	}
	// Worlds are game-agnostic — they stage, they don't overwrite.
	if dir, err := ImportDestDir(g, ImportWorld); err != nil || dir != "imports" {
		t.Errorf("world dest = %q, %v; want imports, nil", dir, err)
	}
}
