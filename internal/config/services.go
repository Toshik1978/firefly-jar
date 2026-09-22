package config

// Firefly is the firefly: section. URL is normalized by Load to the API base ending in /api/v1.
type Firefly struct {
	URL       string `yaml:"url"`
	TokenFile string `yaml:"token_file"`
}

// EnableBanking is the enablebanking: section. AppID is the JWT kid; PSUType is personal or
// business.
type EnableBanking struct {
	AppID          string `yaml:"app_id"`
	PrivateKeyFile string `yaml:"private_key_file"`
	RedirectURL    string `yaml:"redirect_url"`
	PSUType        string `yaml:"psu_type"`
}
