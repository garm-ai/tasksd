package tasks

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	toolv1 "github.com/garm-ai/contracts/garm/tool/v1"

	tasksv1 "github.com/garm-ai/tasksd/gen/garm/tasks/v1"
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

// Contract reads the service descriptor this repository's own contract
// carries — proto/garm/tasks/v1/tasks.proto, generated into gen/.
//
// Read rather than written down: the names, the routes and the wire shape
// all come from the same descriptor this process serves, so a contract that
// moves is a build that changes rather than a table somebody has to
// remember to edit. That was worth having while the proto was in another
// repository and is worth no less now it is in this one: `mise run gen-check`
// keeps gen/ and proto/ in step, and this keeps the routing table in step
// with gen/.
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
// (garm's internal/compiler, emit_micro.go, func descriptorHash). It is
// written here because that function is unexported in another repository's
// internal/ package, which owning the proto does not change: a hash both a
// producer and a consumer compute belongs in the module they share, which is
// `github.com/garm-ai/contracts`, and this becomes an import when it lands
// there. Until then the golden test beside it pins the value, so the two
// implementations cannot drift apart unnoticed — and a drift that did get
// through is loud rather than silent: the daemon refuses to route to a
// service whose shape it cannot confirm, and says so.
//
// It reads field numbers, names, cardinalities and kinds, and never an
// option. That is why moving this proto from the contract module into this
// one did not move the value: `go_package` is an option.
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
