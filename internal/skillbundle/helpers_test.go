package skillbundle

import "yuno/internal/store"

func skillOf(name, desc, body, license string) store.Skill {
	return store.Skill{Name: name, Description: desc, Body: body, License: license}
}
