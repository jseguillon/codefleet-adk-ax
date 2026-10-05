package demo

import "embed"

//go:embed repos skills
var Assets embed.FS

func Read(path string) string {
	b, err := Assets.ReadFile(path)
	if err != nil {
		panic(err)
	}
	return string(b)
}

var RepoFiles = map[string][]string{
	"api":  {"pricing.py", "test_pricing.py"},
	"web":  {"coupon.mjs", "coupon.test.mjs", "index.html"},
	"docs": {"README.md"},
}
