/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"go/format"
	"log"
	"os"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws/session"
	ec22 "github.com/aws/aws-sdk-go/service/ec2"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter/pkg/apis/settings"
	"github.com/aws/karpenter/pkg/providers/pricing"
	"github.com/aws/karpenter/pkg/test"
)

func getAWSRegions(partition string) []string {
	switch partition {
	case "aws":
		return []string{"us-east-1"}
	case "aws-us-gov":
		return []string{"us-gov-east-1", "us-gov-west-1"}
	case "aws-cn":
		return []string{"cn-north-1"}
	default:
		panic("invalid partition")
	}
}

func getPartitionSuffix(partition string) string {
	switch partition {
	case "aws":
		return "AWS"
	case "aws-us-gov":
		return "USGov"
	case "aws-cn":
		return "CN"
	default:
		panic("invalid partition")
	}
}

type Options struct {
	partition string
	output    string
}

func NewOptions() *Options {
	o := &Options{}
	flag.StringVar(&o.partition, "partition", "aws", "The partition to generate prices for. Valid options are \"aws\", \"aws-us-gov\", and \"aws-cn\".")
	flag.StringVar(&o.output, "output", "pkg/providers/pricing/zz_generated.pricing_aws.go", "The destination for the generated go file.")
	flag.Parse()
	if !lo.Contains([]string{"aws", "aws-us-gov", "aws-cn"}, o.partition) {
		log.Fatal("invalid partition: must be \"aws\", \"aws-us-gov\", or \"aws-cn\"")
	}
	return o
}

func main() {
	opts := NewOptions()
	f, err := os.Create("pricing.heapprofile")
	if err != nil {
		log.Fatal("could not create memory profile: ", err)
	}
	defer f.Close() // error handling omitted for example

	const region = "us-east-1"
	os.Setenv("AWS_SDK_LOAD_CONFIG", "true")
	os.Setenv("AWS_REGION", region)
	ctx := context.Background()
	ctx = settings.ToContext(ctx, test.Settings())
	sess := session.Must(session.NewSession())
	ec2 := ec22.New(sess)
	updateStarted := time.Now()
	src := &bytes.Buffer{}
	fmt.Fprintln(src, "//go:build !ignore_autogenerated")
	license := lo.Must(os.ReadFile("hack/boilerplate.go.txt"))
	fmt.Fprintln(src, string(license))
	fmt.Fprintln(src, "package pricing")
	fmt.Fprintln(src, `import "time"`)
	now := time.Now().UTC().Format(time.RFC3339)
	fmt.Fprintf(src, "// generated at %s for %s\n\n\n", now, region)
	fmt.Fprintf(src, "var InitialPriceUpdate%s, _ = time.Parse(time.RFC3339, \"%s\")\n", getPartitionSuffix(opts.partition), now)
	fmt.Fprintf(src, "var InitialOnDemandPrices%s = map[string]map[string]float64{\n", getPartitionSuffix(opts.partition))
	// record prices for each region we are interested in
	for _, region := range getAWSRegions(opts.partition) {
		log.Println("fetching for", region)
		pricingProvider := pricing.NewProvider(ctx, pricing.NewAPI(sess, region), ec2, region)
		controller := pricing.NewController(pricingProvider)
		_, err := controller.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{}})
		if err != nil {
			log.Fatalf("failed to initialize pricing provider %s", err)
		}
		for {
			if pricingProvider.OnDemandLastUpdated().After(updateStarted) && pricingProvider.SpotLastUpdated().After(updateStarted) {
				break
			}
			log.Println("waiting on pricing update...")
			time.Sleep(1 * time.Second)
		}
		instanceTypes := pricingProvider.InstanceTypes()
		sort.Strings(instanceTypes)

		writePricing(src, instanceTypes, region, pricingProvider.OnDemandPrice)
	}
	fmt.Fprintln(src, "}")
	formatted, err := format.Source(src.Bytes())
	if err != nil {
		if err := os.WriteFile(opts.output, src.Bytes(), 0644); err != nil {
			log.Fatalf("writing output, %s", err)
		}
		log.Fatalf("formatting generated source, %s", err)
	}

	if err := os.WriteFile(opts.output, formatted, 0644); err != nil {
		log.Fatalf("writing output, %s", err)
	}
	runtime.GC()
	if err := pprof.WriteHeapProfile(f); err != nil {
		log.Fatal("could not write memory profile: ", err)
	}
}

func writePricing(src *bytes.Buffer, instanceNames []string, region string, getPrice func(instanceType string) (float64, bool)) {
	fmt.Fprintf(src, "// %s\n", region)
	fmt.Fprintf(src, "%q: {\n", region)
	lineLen := 0
	sort.Strings(instanceNames)
	previousFamily := ""
	for _, instanceName := range instanceNames {
		segs := strings.Split(instanceName, ".")
		if len(segs) != 2 {
			log.Fatalf("parsing instance family %s, got %v", instanceName, segs)
		}
		price, ok := getPrice(instanceName)
		if !ok {
			continue
		}

		// separating by family should lead to smaller diffs instead of just breaking at line endings only
		family := segs[0]
		if family != previousFamily {
			previousFamily = family
			newline(src)
			fmt.Fprintf(src, "// %s family\n", family)
			lineLen = 0
		}

		n, err := fmt.Fprintf(src, `"%s":%f, `, instanceName, price)
		if err != nil {
			log.Fatalf("error writing, %s", err)
		}
		lineLen += n
		if lineLen > 80 {
			lineLen = 0
			fmt.Fprintln(src)
		}
	}
	fmt.Fprintln(src, "\n},")
	fmt.Fprintln(src)
}

// newline adds a newline to src, if it does not currently already end with a newline
func newline(src *bytes.Buffer) {
	contents := src.Bytes()
	// no content yet, so create the new line
	if len(contents) == 0 {
		fmt.Println(src)
		return
	}
	// already has a newline, so don't write a new one
	if contents[len(contents)-1] == '\n' {
		return
	}
	fmt.Fprintln(src)
}
