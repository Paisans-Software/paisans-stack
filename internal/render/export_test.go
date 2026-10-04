package render

// SettingOwner is one row of the table of settings that own a rendered key,
// exported so the external tests can check every row is true.
type SettingOwner = settingOwner

func SettingOwners() []SettingOwner { return settingOwners }

// ComposeSets exposes the compose.yaml scan to a test of its own.
var ComposeSets = composeSets
