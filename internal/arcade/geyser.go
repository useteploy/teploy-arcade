package arcade

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Only an enabled regular jar establishes a listener. Configuration is read
// through the held server root with a descriptor/type/byte bound. The admitted
// schema is the scalar bedrock.port mapping; ambiguous YAML is refused.
func detectGeyser(dir string) (int, bool, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, false, err
	}
	defer root.Close()
	plugins, err := openDirIn(root, "plugins")
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer plugins.Close()
	entries, err := plugins.ReadDir(maxListItems + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, false, err
	}
	if len(entries) > maxListItems {
		return 0, false, fmt.Errorf("plugin directory exceeds admission limit")
	}
	var jar string
	for _, e := range entries {
		name := strings.ToLower(e.Name())
		if !strings.HasPrefix(name, "geyser") || !strings.HasSuffix(name, ".jar") {
			continue
		}
		f, _, err := openRegularIn(root, filepath.Join("plugins", e.Name()))
		if err != nil {
			return 0, false, err
		}
		f.Close()
		if jar != "" {
			return 0, false, fmt.Errorf("multiple enabled Geyser jars make listener identity ambiguous")
		}
		jar = e.Name()
	}
	if jar == "" {
		return 0, false, nil
	}
	config := filepath.Join("plugins", strings.TrimSuffix(jar, filepath.Ext(jar)), "config.yml")
	f, st, err := openRegularIn(root, config)
	if os.IsNotExist(err) {
		return geyserDefaultPort, true, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	if st.Size() > 64<<10 {
		return 0, false, fmt.Errorf("Geyser configuration exceeds 64 KiB")
	}
	b, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil {
		return 0, false, err
	}
	if len(b) > 64<<10 {
		return 0, false, fmt.Errorf("Geyser configuration exceeds 64 KiB")
	}
	port, seen, block := geyserDefaultPort, false, false
	blocks := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "\t") {
			return 0, false, fmt.Errorf("Geyser YAML indentation must use spaces")
		}
		trim := strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		if trim == "" {
			continue
		}
		if line == strings.TrimLeft(line, " ") {
			block = trim == "bedrock:"
			if block {
				blocks++
				if blocks > 1 {
					return 0, false, fmt.Errorf("duplicate bedrock mapping")
				}
			}
			continue
		}
		if !block {
			continue
		}
		if value, ok := strings.CutPrefix(trim, "port:"); ok {
			if seen {
				return 0, false, fmt.Errorf("duplicate bedrock.port")
			}
			seen = true
			value = strings.Trim(strings.TrimSpace(value), "\"'")
			port, err = strconv.Atoi(value)
			if err != nil || port < 1 || port > 65535 {
				return 0, false, fmt.Errorf("invalid Geyser bedrock.port")
			}
		}
		if value, ok := strings.CutPrefix(trim, "clone-remote-port:"); ok && strings.TrimSpace(value) != "false" {
			return 0, false, fmt.Errorf("Geyser clone-remote-port is unsupported; configure an explicit bedrock.port")
		}
	}
	return port, true, nil
}
