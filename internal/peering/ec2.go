package peering

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/go-logr/logr"
	seiconfig "github.com/sei-protocol/sei-config"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// EC2 instance tag keys the resolver reads to compose a peer's P2P address.
// They are a forward contract: an EC2-backed sei node publishes its Tendermint
// node_id (mandatory) and optionally a non-default P2P port as instance tags.
// A missing node_id skips the peer, mirroring the Label path.
const (
	tagNodeID  = "sei.io/node-id"
	tagP2PPort = "sei.io/p2p-port"
)

// ec2API is the narrow EC2 surface the resolver needs, declared here so it
// can be mocked. *ec2.Client satisfies it.
type ec2API interface {
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

// EC2Resolver resolves a single EC2Tags source to composed peer strings.
type EC2Resolver interface {
	Resolve(ctx context.Context, src *seiv1alpha1.EC2TagsPeerSource) ([]string, error)
}

// AWSEC2Resolver is the production EC2Resolver. It loads credentials via the
// default chain (IRSA-compatible) and caches a client per region, mirroring
// internal/platform.S3ObjectStore.
type AWSEC2Resolver struct {
	mu      sync.Mutex
	clients map[string]ec2API
	// newClient is overridable in tests; nil uses the real AWS client.
	newClient func(ctx context.Context, region string) (ec2API, error)
}

// NewAWSEC2Resolver returns an EC2Resolver backed by the AWS EC2 API.
func NewAWSEC2Resolver() *AWSEC2Resolver {
	return &AWSEC2Resolver{clients: make(map[string]ec2API)}
}

func (r *AWSEC2Resolver) clientFor(ctx context.Context, region string) (ec2API, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if c, ok := r.clients[region]; ok {
		return c, nil
	}
	build := r.newClient
	if build == nil {
		build = func(ctx context.Context, region string) (ec2API, error) {
			cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
			if err != nil {
				return nil, err
			}
			return ec2.NewFromConfig(cfg), nil
		}
	}
	c, err := build(ctx, region)
	if err != nil {
		return nil, err
	}
	r.clients[region] = c
	return c, nil
}

// Resolve queries EC2 for running instances matching the source's tag filters
// and composes each into `<node_id>@<host>:<port>`. An instance missing the
// node_id tag is skipped (logged) rather than failing the whole source — one
// mis-tagged instance must not wedge peering.
func (r *AWSEC2Resolver) Resolve(ctx context.Context, src *seiv1alpha1.EC2TagsPeerSource) ([]string, error) {
	c, err := r.clientFor(ctx, src.Region)
	if err != nil {
		return nil, fmt.Errorf("loading EC2 client for region %s: %w", src.Region, err)
	}
	return resolveEC2Instances(ctx, c, src)
}

// resolveEC2Instances is the client-agnostic core: it pages DescribeInstances
// for running instances matching src.Tags and composes peer strings. Split out
// from Resolve so tests can drive it with a mock ec2API directly.
func resolveEC2Instances(ctx context.Context, c ec2API, src *seiv1alpha1.EC2TagsPeerSource) ([]string, error) {
	logger := log.FromContext(ctx)

	filters := make([]ec2types.Filter, 0, len(src.Tags)+1)
	filters = append(filters, ec2types.Filter{
		Name:   aws.String("instance-state-name"),
		Values: []string{"running"},
	})
	for k, v := range src.Tags {
		filters = append(filters, ec2types.Filter{
			Name:   aws.String("tag:" + k),
			Values: []string{v},
		})
	}

	var peers []string
	in := &ec2.DescribeInstancesInput{Filters: filters}
	for {
		out, err := c.DescribeInstances(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("describing EC2 instances in %s: %w", src.Region, err)
		}
		for ri := range out.Reservations {
			for ii := range out.Reservations[ri].Instances {
				inst := &out.Reservations[ri].Instances[ii]
				peer, ok := composeEC2Peer(logger, inst, src.Region)
				if !ok {
					continue
				}
				peers = append(peers, peer)
			}
		}
		if out.NextToken == nil {
			return peers, nil
		}
		in.NextToken = out.NextToken
	}
}

// nodeIDPattern is the CometBFT node-id shape: 40 lowercase hex characters
// (the hex-encoded 20-byte address derived from the node's p2p public key).
var nodeIDPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// composeEC2Peer builds `<node_id>@<host>:<port>` from an instance's tags and
// network attributes, returning false (logged) when the instance is unusable.
//
// node_id and host are untrusted tag/attribute input, so both are validated
// before composition: node_id must be the CometBFT 40-hex shape, and a host
// containing a comma, whitespace, or control character is rejected. A comma
// would inject extra persistent_peers entries (the set is joined comma-separated
// downstream); control characters would corrupt config.toml.
func composeEC2Peer(logger logr.Logger, inst *ec2types.Instance, region string) (string, bool) {
	instanceID := aws.ToString(inst.InstanceId)

	tags := make(map[string]string, len(inst.Tags))
	for _, t := range inst.Tags {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	nodeID := tags[tagNodeID]
	if nodeID == "" {
		logger.Info("skipping EC2 instance without node_id tag", "instanceID", instanceID, "region", region)
		return "", false
	}
	if !nodeIDPattern.MatchString(nodeID) {
		logger.Info("skipping EC2 instance with malformed node_id tag (want 40 lowercase hex)",
			"instanceID", instanceID, "region", region, "nodeID", nodeID)
		return "", false
	}

	host := firstNonEmpty(
		aws.ToString(inst.PublicDnsName),
		aws.ToString(inst.PublicIpAddress),
		aws.ToString(inst.PrivateDnsName),
		aws.ToString(inst.PrivateIpAddress),
	)
	if host == "" {
		logger.Info("skipping EC2 instance without a usable host address", "instanceID", instanceID, "region", region)
		return "", false
	}
	if !validHost(host) {
		logger.Info("skipping EC2 instance with unsafe host (comma, whitespace, or control char)",
			"instanceID", instanceID, "region", region, "host", host)
		return "", false
	}

	port := seiconfig.PortP2P
	if p, ok := tags[tagP2PPort]; ok {
		if parsed, err := strconv.ParseInt(p, 10, 32); err == nil && parsed > 0 {
			port = int32(parsed)
		}
	}
	return fmt.Sprintf("%s@%s:%d", nodeID, host, port), true
}

// validHost rejects a comma, whitespace, or control character (see
// composeEC2Peer for why). Intentionally permissive otherwise — host
// resolvability is CometBFT's concern, not ours.
func validHost(host string) bool {
	return !strings.ContainsFunc(host, func(r rune) bool {
		return r == ',' || r <= ' ' || r == 0x7f
	})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// resolveEC2 resolves an EC2Tags source, returning (peers, preservePrior).
// preservePrior is true on a transient resolve failure (or a nil EC2 resolver):
// unlike the Label path, a DescribeInstances failure yields no host list, so
// there is no per-peer entry to selectively preserve. The caller unions the
// node's prior resolved set instead, so persistent_peers does not churn on a
// transient AWS error.
func (r *Resolver) resolveEC2(
	ctx context.Context,
	src *seiv1alpha1.EC2TagsPeerSource,
) (peers []string, preservePrior bool) {
	logger := log.FromContext(ctx)
	if r.EC2 == nil {
		logger.Info("EC2 peer source declared but no EC2 resolver configured; preserving prior peer set", "region", src.Region)
		return nil, true
	}
	resolved, err := r.EC2.Resolve(ctx, src)
	if err != nil {
		logger.Info("EC2 peer resolution failed; preserving prior peer set", "region", src.Region, "err", err)
		return nil, true
	}
	return resolved, false
}
