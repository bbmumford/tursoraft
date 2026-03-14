# MeshDB Data-at-Rest Encryption

## Overview

MeshDB supports **page-level encryption** for embedded replica databases using libraryQL's built-in encryption capabilities. This provides SQLCipher-compatible encryption for local `.db` files while keeping server-side Turso databases encrypted independently.

## ✅ Current Implementation Status

**As of go-libsql v0.0.0-20250912065916**, encryption is **fully supported** via:

- `libsql.WithEncryption(key)` option for embedded replicas
- Page-level AES-256 encryption (SQLCipher compatible)
- Transparent encryption/decryption during database operations
- Compatible with Turso synchronization

## ⚠️ Important Historical Note

Earlier versions of go-libsql did NOT support SQLCipher encryption and would silently ignore `PRAGMA key` commands. **This limitation has been resolved** - encryption now works as documented below.

## Architecture

### Encryption Modes

#### Embedded Replica Mode (with encryption)
- **Build tag**: `-tags libsql_embed`
- **Encryption**: Page-level via `libsql.WithEncryption(key)` 
- **Scope**: Local `.db` files on disk
- **Method**: AES-256 encryption (SQLCipher compatible)
- **Key management**: Application-provided via configuration

#### Remote Mode (server-side encryption)
- **Build tag**: Default (no tags)
- **Encryption**: Server-side by Turso
- **Scope**: Cloud database storage
- **Key management**: Turso-managed
- **Client**: No local encryption (data in transit via TLS)

### Key Properties
- **Length**: 32+ bytes recommended (256-bit minimum)
- **Format**: Raw bytes (not hex-encoded)
- **Storage**: Platform configuration (config-platform.json) or environment variables
- **Rotation**: Requires database re-encryption
- **Transport**: Loaded from platform config at runtime

## Configuration

### Platform Config Approach

Encryption keys are configured in `config-platform.json` under the `database.encryption` section:

```json
{
  "database": {
    "platform": {
      "url": "libsql://core-hstles.turso.io",
      "authToken": "..."
    },
    "tursoAPI": {
      "url": "https://api.turso.sh/v1",
      "token": "..."
    },
    "embeddedReplica": {
      "enabled": true,
      "orgName": "hstles",
      "groupName": "default",
      "dbName": "core"
    },
    "encryption": {
      "platformKey": "",
      "authSessionsKey": ""
    }
  }
}
```

### Key Fields

- **`platformKey`**: Encryption key for platform database (core application data)
  - Used when: PureLocal=false (Turso-backed with optional local replicas)
  - Scope: Persistent application data, user accounts, core business logic
  - Required: No (empty = no encryption)
  - Recommended: 32+ bytes for AES-256

- **`authSessionsKey`**: Encryption key for auth sessions database (2FA/OAuth state)
  - Used when: PureLocal=true (pure local + Raft coordination)
  - Scope: Ephemeral auth state (2FA codes, OAuth handshake tokens)
  - Required: No (empty = no encryption)
  - Recommended: 32+ bytes for AES-256

### Security Considerations

⚠️ **Important**: Even though keys are in `config-platform.json`, treat this file as sensitive:

1. **Private repository**: Keep the repository private
2. **Access control**: Limit who can view/edit the config
3. **Environment secrets**: For production, consider loading from Fly.io secrets
4. **Key rotation**: Generate new keys regularly and re-encrypt databases
5. **Audit trail**: Track changes to encryption keys in version control

### Example Configurations

#### Development (No Encryption)
```json
{
  "encryption": {
    "platformKey": "",
    "authSessionsKey": ""
  }
}
```

#### Production (With Encryption)
```json
{
  "encryption": {
    "platformKey": "your-32-byte-base64-encoded-key-here",
    "authSessionsKey": "different-32-byte-key-for-sessions"
  }
}
```

### Generating Encryption Keys

Use `openssl` to generate secure 32-byte keys:

```bash
# Generate platform key
openssl rand -base64 32

# Generate auth sessions key (use different key)
openssl rand -base64 32
```

### GroupConfig Structure (Internal)

The encryption keys flow through the internal GroupConfig structure:

```go
type GroupConfig struct {
    GroupName     string   `json:"group_name"`
    AuthToken     string   `json:"auth_token"`
    DBNames       []string `json:"db_names"`
    EncryptionKey string   `json:"-"` // Not serialized to JSON
    PureLocal     bool     `json:"pure_local"`
}
```

Note: `EncryptionKey` is loaded at runtime from platform config, not directly from MeshDB config JSON.


## Implementation Details

### Connector Creation
The encryption key is passed through the connector creation flow:

```go
// manager.go - NewManager initialization
encryptionKey := group.EncryptionKey
handle, err := makeConnector(dbPath, primaryURL, authToken, encryptionKey)
```

### Build Tag Handling

**Embedded Build** (`connector_embed.go`):
```go
//go:build libsql_embed

func makeConnector(dbPath, primaryURL, authToken, encryptionKey string) (*databaseHandle, error) {
    opts := []libsql.Option{libsql.WithAuthToken(authToken)}
    
    // Apply encryption if key provided
    if encryptionKey != "" {
        opts = append(opts, libsql.WithEncryption(encryptionKey))
    }
    
    connector, err := libsql.NewEmbeddedReplicaConnector(dbPath, primaryURL, opts...)
    // ...
}
```

**Remote Build** (`connector_remote.go`):
```go
//go:build !libsql_embed

func makeConnector(dbPath, primaryURL, authToken, encryptionKey string) (*databaseHandle, error) {
    // Note: encryptionKey ignored - server-side encryption by Turso
    // Parameter included for function signature compatibility
    
    dsn := fmt.Sprintf("%s?authToken=%s", primaryURL, authToken)
    db, err := sql.Open("libsql", dsn)
    // ...
}
```

## Build Requirements

### Embedded Mode with Encryption
```bash
# Requires CGO for libraryQL embedded library
export CGO_ENABLED=1
go build -tags libsql_embed -o meshdb-node ./cmd/node.hstles.com
```

### Remote Mode (No Local Encryption)
```bash
# No CGO required
go build -o meshdb-node ./cmd/node.hstles.com
```

### Cross-Platform Builds
```bash
# macOS (arm64)
GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 go build -tags libsql_embed

# Linux (amd64)
GOOS=linux GOARCH=amd64 CGO_ENABLED=1 go build -tags libsql_embed

# Windows (Note: Requires MinGW/TDM-GCC for CGO)
GOOS=windows GOARCH=amd64 CGO_ENABLED=1 go build -tags libsql_embed
```
## Key Management

### Environment Variable Approach
```bash
# Load from KMS and set environment variable
export MESHDB_ENCRYPTION_KEY=$(aws kms decrypt --ciphertext-blob ...)

# Application reads from environment
encryptionKey := os.Getenv("MESHDB_ENCRYPTION_KEY")
```

### Configuration File with KMS
```go
// Load encrypted config, decrypt with KMS
encryptedConfig := loadConfig("meshdb.json.encrypted")
decryptedKey, err := kms.Decrypt(encryptedConfig.EncryptionKey)

cfg := &Config{
    Groups: []GroupConfig{
        {
            GroupName:     "production",
            EncryptionKey: decryptedKey,
        },
    },
}
```

### Key Rotation Strategy
1. **Create new key** in KMS
2. **Deploy new key** via config update
3. **Re-encrypt databases**:
   ```sql
   PRAGMA rekey = '<new-encryption-key>';
   ```
4. **Verify** new key works
5. **Retire old key** after grace period

## Security Considerations

### Key Storage
- ✅ **DO**: Store in KMS (AWS KMS, HashiCorp Vault, Azure Key Vault)
- ✅ **DO**: Use environment variables loaded from secure stores
- ✅ **DO**: Implement key rotation schedule
- ❌ **DON'T**: Hardcode keys in source code
- ❌ **DON'T**: Commit keys to version control
- ❌ **DON'T**: Store keys in plaintext config files

### Access Control
- Limit key access to MeshDB process only
- Use OS-level permissions on key files (if file-based)
- Audit key access and usage
- Implement least-privilege access policies

### Data Protection
- **At rest**: Encrypted via SQLCipher when key provided
- **In transit**: TLS for Turso synchronization
- **In memory**: Key remains in process memory (use secure memory wiping if possible)

## Performance Impact

### Encryption Overhead
- **CPU**: ~5-10% overhead for encryption/decryption
- **Disk I/O**: Page-level encryption (minimal overhead)
- **Memory**: Slight increase for encryption buffers

### Optimization Tips
- Use larger page sizes (4096 bytes recommended)
- Batch writes to minimize encryption operations
- Consider encryption only for sensitive databases
- Monitor CPU usage in production

## Troubleshooting

### Build Errors

**Error**: `library 'sql_experimental' not found`
- **Cause**: CGO not enabled or libraryQL library missing
- **Fix**: `export CGO_ENABLED=1` and rebuild

**Error**: `undefined: libsql.WithEncryption`
- **Cause**: Old libraryQL version
- **Fix**: `go get -u github.com/tursodatabase/go-libsql`

### Runtime Errors

**Error**: `file is not a database`
- **Cause**: Wrong encryption key or corrupted database
- **Fix**: Verify key matches database encryption; restore from backup if corrupted

**Error**: `database is locked`
- **Cause**: Multiple processes accessing encrypted database
- **Fix**: Ensure single-writer access; use WAL mode with encryption

### Performance Issues

**Symptom**: High CPU usage during queries
- **Check**: Encryption overhead normal (5-10%)
- **Action**: Profile with `pprof` to identify bottlenecks
- **Consider**: Disable encryption for non-sensitive data

## Testing

### Unit Tests
```go
func TestEncryptedConnector(t *testing.T) {
    key := make([]byte, 32)
    rand.Read(key)
    
    dbPath := filepath.Join(t.TempDir(), "test.db")
    handle, err := makeConnector(dbPath, primaryURL, authToken, string(key))
    require.NoError(t, err)
    
    // Test write
    _, err = handle.db.Exec("CREATE TABLE test (id INTEGER PRIMARY KEY)")
    require.NoError(t, err)
    
    // Verify encryption by trying to open without key
    rawDB, err := sql.Open("libsql", "file:"+dbPath)
    require.NoError(t, err)
    _, err = rawDB.Exec("SELECT * FROM test")
    require.Error(t, err) // Should fail without encryption key
}
```

### Integration Tests
```bash
# Start encrypted MeshDB node
export MESHDB_ENCRYPTION_KEY=$(openssl rand -base64 32)
go run -tags libsql_embed cmd/node.hstles.com/main.go

# Verify encryption
sqlite3 /data/meshdb/production/core.db "SELECT * FROM sessions"
# Should fail: "file is not a database" or "database is encrypted"
```

## Migration Guide

### Adding Encryption to Existing Databases

#### Step 1: Backup Data
```bash
# Create unencrypted backup
sqlite3 production.db ".backup production.backup.db"
```

#### Step 2: Generate Encryption Key
```bash
# Generate 32-byte key
openssl rand -base64 32 > encryption.key

# Store in KMS
aws kms encrypt --key-id alias/meshdb --plaintext fileb://encryption.key
```

#### Step 3: Encrypt Database
```sql
-- Open unencrypted database
ATTACH DATABASE 'production.db' AS plaintext KEY '';

-- Create encrypted copy
ATTACH DATABASE 'production.encrypted.db' AS encrypted KEY '<encryption-key>';

-- Copy data
SELECT sqlcipher_export('encrypted');

-- Detach and verify
DETACH DATABASE plaintext;
DETACH DATABASE encrypted;
```

#### Step 4: Update Configuration
```json
{
  "groups": [
    {
      "group_name": "production",
      "encryption_key": "LOAD_FROM_KMS"
    }
  ]
}
```

#### Step 5: Deploy and Verify
```bash
# Deploy with encryption enabled
CGO_ENABLED=1 go build -tags libsql_embed

# Verify queries work
curl -H "X-API-Key: $API_KEY" https://mesh.hstles.com/health
```

## Alternative Encryption Options

### Filesystem-Level Encryption

**Use OS-level encryption for database directories when database-level encryption is not needed:**

#### Linux (dm-crypt/LUKS)
```bash
# Create encrypted volume
cryptsetup luksFormat /dev/sdb1
cryptsetup open /dev/sdb1 meshdb_encrypted

# Mount and use
mkfs.ext4 /dev/mapper/meshdb_encrypted
mount /dev/mapper/meshdb_encrypted /var/lib/meshdb
```

#### macOS (FileVault)
```bash
# Enable FileVault for entire disk
fdesetup enable

# Or use encrypted sparse bundle
hdiutil create -size 10g -type SPARSEBUNDLE \
    -encryption AES-256 -fs "Case-sensitive APFS" \
    meshdb_encrypted.sparsebundle
```

#### Windows (BitLocker)
```powershell
# Enable BitLocker for drive
Enable-BitLocker -MountPoint "D:" -EncryptionMethod Aes256
```

**Pros:**
- ✅ Works with any database driver
- ✅ Transparent to application
- ✅ OS-level key management
- ✅ Proven security

**Cons:**
- ❌ Requires admin/root privileges
- ❌ Key management is OS-specific
- ❌ Cannot encrypt individual databases (entire volume)

## References

- **libraryQL Documentation**: https://docs.turso.tech/libsql
- **go-libsql GitHub**: https://github.com/tursodatabase/go-libsql
- **SQLCipher Design**: https://www.zetetic.net/sqlcipher/design/
- **Turso Encryption**: https://docs.turso.tech/features/encryption

---
**Last Updated**: 2025-01-16  
**Maintainers**: Update when modifying encryption implementation or key management procedures


### 4. Compliance and Audit

#### Log Encryption Status
```go
db, err := meshdb.NewLocalRaftDatabase(cfg)
if err != nil {
    log.Fatal(err)
}

status := meshdb.EncryptionStatus(db)
log.Printf("Database encryption status: %s", status)
// Output: "not encrypted (SQLCipher not available - using plain SQLite)"
```

#### Verify Encryption
```go
encrypted, err := meshdb.VerifyEncryption(db)
if err != nil {
    log.Printf("Error verifying encryption: %v", err)
}

if !encrypted && cfg.Encryption.Enabled {
    log.Printf("WARNING: Encryption requested but not active (driver limitation)")
    log.Printf("RECOMMENDATION: Use filesystem-level encryption")
}
```

## Testing Encryption

### Unit Tests

```go
func TestEncryptionConfig(t *testing.T) {
    // Test key generation
    cfg := meshdb.DefaultEncryptionConfig()
    cfg.Enabled = true
    
    key, err := cfg.GenerateKey()
    if err != nil {
        t.Fatalf("generate key: %v", err)
    }
    
    if len(key) != 64 { // 32 bytes = 64 hex chars
        t.Errorf("expected 64-char hex key, got %d", len(key))
    }
}

func TestEncryptionFallback(t *testing.T) {
    // Verify graceful fallback when encryption not supported
    cfg := meshdb.LocalRaftConfig{
        LocalDir:     t.TempDir(),
        DBName:       "test",
        RaftBindAddr: ":0",
        Encryption: meshdb.EncryptionConfig{
            Enabled: true,
            Key:     "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
        },
    }
    
    db, err := meshdb.NewLocalRaftDatabase(cfg)
    if err != nil {
        t.Fatalf("open database: %v", err)
    }
    defer db.Close()
    
    // Should work even though encryption isn't really enabled
    var result int
    err = db.QueryRow("SELECT 1").Scan(&result)
    if err != nil {
        t.Errorf("query failed: %v", err)
    }
    
    // Verify encryption status
    encrypted, _ := meshdb.VerifyEncryption(db)
    if encrypted {
        t.Errorf("encryption reported as active (should not be with go-libsql)")
    }
}
```

## Troubleshooting

### Encryption Not Working

**Symptom**: Database files are plaintext despite encryption configuration

**Cause**: go-libsql doesn't support SQLCipher

**Solution**: Use filesystem-level encryption (see Option 1 above)

### Key Not Found

**Symptom**: `no encryption key configured` error

**Solution**: Verify key source is correctly configured
```bash
# Check environment variable
echo $MESHDB_ENCRYPTION_KEY

# Check file exists and is readable
cat /etc/secrets/meshdb.key
```

### Wrong Key Size

**Symptom**: Encryption fails with key size error

**Solution**: Use 256-bit (32-byte) keys
```go
cfg.Encryption.KeySize = 32 // Required for AES-256
```

### Permission Denied

**Symptom**: Cannot read key file

**Solution**: Fix file permissions
```bash
chmod 600 /etc/secrets/meshdb.key
chown meshdb:meshdb /etc/secrets/meshdb.key
```

## Future Work

### Planned Features

- [ ] **SQLCipher Driver Integration**: When go-libsql adds SQLCipher support
- [ ] **Key Rotation**: `PRAGMA rekey` support for changing encryption keys
- [ ] **Multi-Key Support**: Different keys for different databases/tenants
- [ ] **Hardware Security Module (HSM)**: Integration with cloud KMS (AWS KMS, Azure Key Vault, etc.)
- [ ] **Envelope Encryption**: Encrypt data keys with master keys

### Monitoring Upstream

Watch these repositories for SQLCipher support:

- **go-libsql**: https://github.com/tursodatabase/go-libsql
- **libraryQL**: https://github.com/tursodatabase/libsql

## References

- **SQLCipher Documentation**: https://www.zetetic.net/sqlcipher/
- **LUKS/dm-crypt**: https://gitlab.com/cryptsetup/cryptsetup
- **FileVault**: https://support.apple.com/guide/mac-help/mh11785/mac
- **BitLocker**: https://docs.microsoft.com/en-us/windows/security/information-protection/bitlocker/

---

**Last Updated**: 2025-01-18  
**Status**: Encryption interface ready; awaiting driver support  
**Maintainer**: Update when SQLCipher support becomes available in go-libsql
