package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// The application-menu entry for Studio. It runs this same executable with
// --app (its own window), --workspace (a folder of workflows, so no folder has
// to be chosen first) and --quit-when-closed (there is no terminal to stop it).

const (
	launcherName = "nexusrun-studio"
	// launcherIcon is the mark: two linked nodes, the second one orange.
	launcherIcon = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 128 128" width="128" height="128">
<rect width="128" height="128" rx="28" fill="#1d1d1f"/>
<path d="M44 64h40" stroke="#f5f5f7" stroke-width="7" stroke-linecap="round"/>
<circle cx="40" cy="64" r="17" fill="none" stroke="#f5f5f7" stroke-width="7"/>
<circle cx="88" cy="64" r="17" fill="#ff7a1a"/>
</svg>
`
)

func defaultWorkspace() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "NexusRun"), nil
}

// desktopQuote quotes one argument for the Exec key of a desktop entry. Inside
// double quotes a double quote, backtick, dollar sign and backslash are each
// escaped with a backslash; the file format then reads backslash as an escape
// itself, so every one of those backslashes is written twice (a literal
// backslash becomes four). A percent sign is doubled so it is not a field code.
func desktopQuote(arg string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range arg {
		switch r {
		case '\\':
			b.WriteString(`\\\\`)
		case '"', '`', '$':
			b.WriteString(`\\`)
			b.WriteRune(r)
		case '%':
			b.WriteString("%%")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func desktopEntry(exe, workspace string) string {
	return "[Desktop Entry]\n" +
		"Type=Application\n" +
		"Name=NexusRun Studio\n" +
		"Comment=Build AI workflows from your local models\n" +
		"Exec=" + desktopQuote(exe) + " studio --app --quit-when-closed --workspace " + desktopQuote(workspace) + "\n" +
		"Icon=" + launcherName + "\n" +
		"Terminal=false\n" +
		"Categories=Development;Utility;\n" +
		"Keywords=AI;workflow;agents;models;\n" +
		"StartupNotify=true\n"
}

func dataHome() (string, error) {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" && filepath.IsAbs(x) {
		return x, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share"), nil
}

func launcherPaths() (desktop, icon string, err error) {
	data, err := dataHome()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(data, "applications", launcherName+".desktop"),
		filepath.Join(data, "icons", "hicolor", "scalable", "apps", launcherName+".svg"), nil
}

func launcherCommand(workspace string, remove bool) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("the application menu entry is available on Linux; on %s run `%s studio --app --workspace <folder>`", runtime.GOOS, binName)
	}
	desktop, icon, err := launcherPaths()
	if err != nil {
		return err
	}
	if remove {
		for _, p := range []string{desktop, icon} {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		refreshDesktopDatabase(filepath.Dir(desktop))
		fmt.Println("Removed NexusRun Studio from the application menu.")
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	if workspace == "" {
		if workspace, err = defaultWorkspace(); err != nil {
			return err
		}
	}
	if workspace, err = filepath.Abs(workspace); err != nil {
		return err
	}
	for _, f := range []struct {
		path, body string
		mode       os.FileMode
	}{
		{icon, launcherIcon, 0o644},
		{desktop, desktopEntry(exe, workspace), 0o644},
	} {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(f.path, []byte(f.body), f.mode); err != nil {
			return err
		}
	}
	refreshDesktopDatabase(filepath.Dir(desktop))
	fmt.Printf("Added NexusRun Studio to the application menu.\n  workflows are kept in %s\n  remove it with: %s studio --remove-launcher\n", workspace, binName)
	return nil
}

// refreshDesktopDatabase asks the desktop to notice the new entry. Menus that
// watch the folder find it anyway, so a missing tool is not an error.
func refreshDesktopDatabase(dir string) {
	if p, err := exec.LookPath("update-desktop-database"); err == nil {
		_ = exec.Command(p, dir).Run()
	}
}
