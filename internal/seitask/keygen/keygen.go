// Package keygen implements `seitask keygen`: generate a BIP-39 mnemonic +
// cosmos secp256k1 keypair, write the mnemonic to a per-run Secret named
// "<keyName>-<workflowName>", and publish ADMIN_ADDRESS / ADMIN_SECRET_NAME
// to workflow-vars. All created resources carry an ownerRef to the parent
// Workflow CR for cascade GC.
package keygen

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/cosmos/btcutil/bech32"
	bip39 "github.com/cosmos/go-bip39"
	"golang.org/x/crypto/ripemd160" //nolint:staticcheck // Cosmos address derivation is bound to RIPEMD-160 by protocol.

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sei-protocol/sei-k8s-controller/internal/taskimg"
)

// cosmos BIP-44 path, coin type 118. Matches `seid keys add` so mnemonics
// generated here import verbatim into a seid keyring.
const cosmosHDPath = "m/44'/118'/0'/0/0"

const bech32AccountPrefix = "sei"

// SecretMnemonicKey is the data key downstream pods reference via secretKeyRef.
const SecretMnemonicKey = "mnemonic"

const fieldOwner client.FieldOwner = "seitask-keygen"

// Params carries the typed inputs to Run.
type Params struct {
	// KeyName is the logical identity (e.g. "admin"). Secret name is
	// "<KeyName>-<WorkflowName>" to disambiguate concurrent runs.
	KeyName  string
	Workflow taskimg.WorkflowIdentity
}

type Result struct {
	SecretName string
	Address    string
}

// Run generates the keypair, writes the Secret, and stamps workflow-vars.
// Idempotent: re-running on an existing Secret reuses the key.
func Run(ctx context.Context, c client.Client, p Params) (Result, error) {
	if p.KeyName == "" {
		return Result{}, fmt.Errorf("keygen: empty KeyName")
	}
	if p.Workflow.Name == "" || p.Workflow.Namespace == "" {
		return Result{}, fmt.Errorf("keygen: workflow identity not loaded (downward-API env not projected)")
	}

	secretName := p.KeyName + "-" + p.Workflow.Name

	// Check for an existing Secret first — re-running keygen on an already-
	// initialized run should be a no-op so manual retries don't rotate the
	// key out from under downstream steps.
	existing := &corev1.Secret{}
	err := c.Get(ctx, client.ObjectKey{Namespace: p.Workflow.Namespace, Name: secretName}, existing)
	switch {
	case err == nil:
		// Re-stamp the workflow-vars CM in case it was cleared, then return.
		addr, exists := existing.Data["address"]
		if !exists {
			return Result{}, taskimg.Infra(fmt.Errorf("existing Secret %q is missing address data", secretName))
		}
		if err := writeWorkflowVars(ctx, c, p.Workflow, string(addr), secretName); err != nil {
			return Result{}, err
		}
		return Result{SecretName: secretName, Address: string(addr)}, nil
	case !apierrors.IsNotFound(err):
		return Result{}, taskimg.Infra(fmt.Errorf("reading existing Secret %q: %w", secretName, err))
	}

	mnemonic, address, err := deriveIdentity()
	if err != nil {
		return Result{}, taskimg.Infra(fmt.Errorf("deriving identity: %w", err))
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            secretName,
			Namespace:       p.Workflow.Namespace,
			OwnerReferences: []metav1.OwnerReference{p.Workflow.OwnerRef()},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			SecretMnemonicKey: []byte(mnemonic),
			// address is duplicated into the Secret so a re-run of keygen
			// can reuse the existing identity without re-deriving from the
			// mnemonic (the Secret is the source of truth for both).
			"address": []byte(address),
		},
	}
	if err := c.Create(ctx, secret, fieldOwner); err != nil {
		// Race: another keygen Pod won. Re-read and fall through to
		// idempotent path.
		if apierrors.IsAlreadyExists(err) {
			return Run(ctx, c, p)
		}
		return Result{}, taskimg.Infra(fmt.Errorf("creating Secret %q: %w", secretName, err))
	}

	if err := writeWorkflowVars(ctx, c, p.Workflow, address, secretName); err != nil {
		return Result{}, err
	}
	return Result{SecretName: secretName, Address: address}, nil
}

func writeWorkflowVars(ctx context.Context, c client.Client, w taskimg.WorkflowIdentity, address, secretName string) error {
	if err := taskimg.EnsureWorkflowVarsCM(ctx, c, w, map[taskimg.VarKey]string{
		taskimg.KeyRunID: w.Name,
	}); err != nil {
		return err
	}
	return taskimg.SetVars(ctx, c, w, map[taskimg.VarKey]string{
		taskimg.KeyAdminAddress:    address,
		taskimg.KeyAdminSecretName: secretName,
	})
}

// deriveIdentity generates a 24-word BIP-39 mnemonic and derives the cosmos
// secp256k1 address at m/44'/118'/0'/0/0. The full pipeline (entropy →
// mnemonic → seed → BIP-32 master → BIP-44 child → secp256k1 → ripemd160
// → bech32) matches `seid keys add` byte-for-byte.
func deriveIdentity() (mnemonic, address string, err error) {
	// 24 words → 256 bits entropy; matches seid default.
	entropy, err := bip39.NewEntropy(256)
	if err != nil {
		return "", "", fmt.Errorf("entropy: %w", err)
	}
	mnemonic, err = bip39.NewMnemonic(entropy)
	if err != nil {
		return "", "", fmt.Errorf("mnemonic: %w", err)
	}

	// BIP-39 PBKDF2 → 64-byte seed; empty passphrase matches seid default.
	seed, err := bip39.NewSeedWithErrorChecking(mnemonic, "")
	if err != nil {
		return "", "", fmt.Errorf("seed: %w", err)
	}
	master, chainCode := computeMasterFromSeed(seed)
	privKey, err := derivePrivateKeyForPath(master, chainCode, cosmosHDPath)
	if err != nil {
		return "", "", fmt.Errorf("derive %s: %w", cosmosHDPath, err)
	}
	_, pub := btcec.PrivKeyFromBytes(privKey)
	pubCompressed := pub.SerializeCompressed()

	// Cosmos address = ripemd160(sha256(pubkey_compressed)), bech32-encoded.
	sha := sha256.Sum256(pubCompressed)
	hasher := ripemd160.New()
	if _, err := hasher.Write(sha[:]); err != nil {
		return "", "", fmt.Errorf("ripemd160: %w", err)
	}
	addrBytes := hasher.Sum(nil)

	converted, err := bech32.ConvertBits(addrBytes, 8, 5, true)
	if err != nil {
		return "", "", fmt.Errorf("bech32 convert: %w", err)
	}
	address, err = bech32.Encode(bech32AccountPrefix, converted)
	if err != nil {
		return "", "", fmt.Errorf("bech32 encode: %w", err)
	}
	return mnemonic, address, nil
}
