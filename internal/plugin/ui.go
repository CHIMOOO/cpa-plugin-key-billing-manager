package plugin

import (
	"bytes"
	"embed"
	"encoding/json"
)

//go:embed ui.html i18n.js locales/*.json
var uiFiles embed.FS

// Assemble once. The browser receives one self-contained HTML resource.
var uiHTML = buildUI()

func buildUI() []byte {
	read := func(name string) []byte {
		data, err := uiFiles.ReadFile(name)
		if err != nil {
			panic(err)
		}
		return data
	}
	catalogs := map[string]map[string]string{}
	for _, language := range []string{"en", "zh-CN"} {
		var entries map[string]string
		if err := json.Unmarshal(read("locales/"+language+".json"), &entries); err != nil {
			panic(err)
		}
		catalogs[language] = entries
	}
	data, err := json.Marshal(catalogs)
	if err != nil {
		panic(err)
	}
	script := append([]byte("const BILLING_MESSAGES = "), data...)
	script = append(script, ';', '\n')
	script = append(script, read("i18n.js")...)
	return bytes.Replace(read("ui.html"), []byte("// BILLING_I18N"), script, 1)
}
