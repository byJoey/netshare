package webui

import "embed"

// FS 包含管理台运行需要的全部静态资源。
//
//go:embed assets/*
var FS embed.FS
