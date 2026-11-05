package cron

import "errors"

// 调度器公开错误。
var (
	ErrJobNotFound = errors.New("定时任务不存在")
	ErrBusy        = errors.New("该任务正在运行中，请稍后再试")
)
