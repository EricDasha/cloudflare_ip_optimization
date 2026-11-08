//go:build !debugapi

package main

import "net/http"

// registerDebugRoutes 在普通构建里是空实现：/api/debug/ 这一组接口
// 不会被注册，请求落到静态处理器上，返回 404。
//
// 这个空实现不是「关掉开关」，而是「代码根本不存在」。生产镜像由
// scripts/build-dist.sh 构建，不带 -tags debugapi，因此调试接口连
// 二进制里都没有。详见 web/debugapi_on.go 顶部的说明。
func registerDebugRoutes(_ *http.ServeMux, _ *app) {}
