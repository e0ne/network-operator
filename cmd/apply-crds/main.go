/*
2024 NVIDIA CORPORATION & AFFILIATES

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package main

import (
	"github.com/NVIDIA/k8s-operator-libs/pkg/crdutil"
)

func main() {
	// Run CRD ensure logic at the end
	crdutil.EnsureCRDsCmd()
}
