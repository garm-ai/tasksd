package tasks

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	tasksv1 "github.com/garm-ai/garm/contracts/garm/tasks/v1"
	toolv1 "github.com/garm-ai/garm/contracts/garm/tool/v1"
)

// ServiceName is the proto service this process implements.
const ServiceName = "garm.tasks.v1.TasksService"

// Tool is one method of the contract, as the catalogue names it.
type Tool struct {
	// FQN is the identity a manifest pins and a ledger row records:
	// the proto package and the tool's declared name.
	FQN string
	// Method is the proto method name, and FullMethod the route the daemon
	// resolves to a subject.
	Method     string
	FullMethod string
}

// Contract reads the service descriptor the linked contract carries.
//
// Read rather than written down: the names, the routes and the wire shape
// all come from the same descriptor this process serves, so a contract that
// moves is a build that changes rather than a table somebody has to
// remember to edit.
func Contract() ([]Tool, error) {
	sd, err := serviceDescriptor()
	if err != nil {
		return nil, err
	}
	pkg := string(sd.ParentFile().Package())
	methods := sd.Methods()
	out := make([]Tool, 0, methods.Len())
	for i := 0; i < methods.Len(); i++ {
		md := methods.Get(i)
		p, ok := proto.GetExtension(md.Options(), toolv1.E_Tool).(*toolv1.ToolPolicy)
		if !ok || p == nil || p.GetName() == "" {
			return nil, fmt.Errorf("tasks: %s declares no tool", md.FullName())
		}
		out = append(out, Tool{
			FQN:        pkg + "." + p.GetName(),
			Method:     string(md.Name()),
			FullMethod: "/" + string(sd.FullName()) + "/" + string(md.Name()),
		})
	}
	return out, nil
}

// DescriptorHash is the wire shape this process implements, as the daemon's
// reconciler compares it against the catalogue.
//
// It covers the request and response messages of every tool in the package,
// and nothing else: not comments, not the policy annotations — so it changes
// when a caller would have to care and stays put when only prose did.
//
// The encoding is the one `garm catalogue build` stamps into a catalogue
// (garm's internal/compiler, emit_micro.go). It is written here because that
// function is not exported and a generated binding for this contract does
// not exist; the right home for it is garm's contracts module, beside the
// grant verification that moved there, and this becomes an import when it
// lands. Until then the golden test below is what pins it, and a mismatch
// is loud rather than silent: the daemon refuses to route to a service whose
// shape it cannot confirm, and says so.
func DescriptorHash() (string, error) {
	sd, err := serviceDescriptor()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	visited := map[protoreflect.FullName]bool{}

	var walk func(md protoreflect.MessageDescriptor)
	walk = func(md protoreflect.MessageDescriptor) {
		if visited[md.FullName()] {
			return
		}
		visited[md.FullName()] = true

		fields := md.Fields()
		ordered := make([]protoreflect.FieldDescriptor, fields.Len())
		for i := range ordered {
			ordered[i] = fields.Get(i)
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].Number() < ordered[j].Number() })

		fmt.Fprintf(h, "message %s\n", md.FullName())
		for _, fd := range ordered {
			fmt.Fprintf(h, "field %d %s %s %s\n", fd.Number(), fd.Name(), fd.Cardinality(), fd.Kind())
		}
		// Recurse after this message's own field list is written, so one
		// message's shape is never interleaved with another's.
		for _, fd := range ordered {
			if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
				walk(fd.Message())
			}
		}
	}

	methods := sd.Methods()
	for i := 0; i < methods.Len(); i++ {
		walk(methods.Get(i).Input())
		walk(methods.Get(i).Output())
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func serviceDescriptor() (protoreflect.ServiceDescriptor, error) {
	fd := (&tasksv1.CreateTaskRequest{}).ProtoReflect().Descriptor().ParentFile()
	sd := fd.Services().ByName("TasksService")
	if sd == nil {
		return nil, fmt.Errorf("tasks: %s declares no TasksService", fd.Path())
	}
	return sd, nil
}
