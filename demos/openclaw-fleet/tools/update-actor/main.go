// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command update-actor repoints an existing (suspended) actor at a different
// ActorTemplate via the UpdateActor RPC. This is the only fleet-update
// primitive Substrate has today — ActorTemplates are immutable, so an image
// or config change means creating a template v2 and repointing every actor at
// it, which discards the actor's memory snapshot (the next restore is
// data-only) while durable and external volumes survive.
//
// kubectl-ate has no `update actor` verb yet, which is why this demo carries
// its own; when the CLI grows one, delete this tool.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/agent-substrate/substrate/internal/ateclient"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

func main() {
	atespace := flag.String("atespace", "", "Atespace the actor lives in (required)")
	templateRef := flag.String("template-ref", "", "Name of the ActorTemplate to repoint the actor at, resolved in the actor's atespace (required)")
	kubeconfig := flag.String("kubeconfig", "", "Path to kubeconfig (default: standard loading rules)")
	k8sContext := flag.String("context", "", "Kubeconfig context to use")
	endpoint := flag.String("endpoint", "", "ate-api-server endpoint (default: port-forward via the kubeconfig)")
	tokenFile := flag.String("token-file", "", "Path to a bearer token file")
	flag.Parse()

	if flag.NArg() != 1 || *atespace == "" || *templateRef == "" {
		fmt.Fprintf(os.Stderr, "usage: update-actor --atespace <atespace> --template-ref <template> <actor-name>\n")
		os.Exit(2)
	}
	actorName := flag.Arg(0)

	ctx := context.Background()
	client, err := ateclient.NewClient(ctx, *kubeconfig, *k8sContext, *endpoint, *tokenFile, false)
	if err != nil {
		log.Fatalf("connecting to ate-api-server: %v", err)
	}
	defer client.Close()

	actor, err := client.GetActor(ctx, &ateapipb.GetActorRequest{
		Actor: &ateapipb.ObjectRef{Atespace: *atespace, Name: actorName},
	})
	if err != nil {
		log.Fatalf("GetActor %s/%s: %v", *atespace, actorName, err)
	}

	actor.ActorTemplate = &ateapipb.ObjectRef{Atespace: *atespace, Name: *templateRef}
	if _, err := client.UpdateActor(ctx, &ateapipb.UpdateActorRequest{Actor: actor}); err != nil {
		log.Fatalf("UpdateActor %s/%s -> template %s: %v", *atespace, actorName, *templateRef, err)
	}
	fmt.Printf("actor %s/%s repointed at template %s (memory state will be discarded on next resume; volumes survive)\n",
		*atespace, actorName, *templateRef)
}
