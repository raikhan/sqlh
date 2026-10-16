package config

import "testing"

func TestKeyHelp(t *testing.T) {
	for a := range DefaultPanelKeys {
		if PanelActionHelp[a] == "" {
			t.Errorf("panel action %s has no help", a)
		}
	}
	for a := range DefaultPickerKeys {
		if PickerActionHelp[a] == "" {
			t.Errorf("picker action %s has no help", a)
		}
	}
}
