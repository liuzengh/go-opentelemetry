//
//
// Tencent is pleased to support the open source community by making tRPC available.
//
// Copyright (C) 2023 THL A29 Limited, a Tencent company.
// All rights reserved.
//
// If you have downloaded a copy of the tRPC source code from Tencent,
// please note that tRPC source code is licensed under the  Apache 2.0 License,
// A copy of the Apache 2.0 License is included in this file.
//
//

package log

import (
	"trpc-system/go-opentelemetry/oteltrpc/logs"
)

var (
	DebugContext  = logs.Debug
	DebugContextf = logs.Debugf

	InfoContext  = logs.Info
	InfoContextf = logs.Infof

	WarnContext  = logs.Warn
	WarnContextf = logs.Warnf

	ErrorContext  = logs.Error
	ErrorContextf = logs.Errorf

	FatalContext  = logs.Fatal
	FatalContextf = logs.Fatalf
)
