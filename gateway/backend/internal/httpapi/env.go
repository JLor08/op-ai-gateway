// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import "os"

func getenv(key string) string { return os.Getenv(key) }
