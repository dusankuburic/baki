# Modular Multi-Platform Architecture with Team Collaboration - DEEP DIVE

## Context

Current architecture is well-structured for single-user desktop but requires significant refactoring to support:
- **Multi-platform deployment**: Tauri desktop (local) + Azure web hosting (cloud)
- **Team collaboration**: Shared libraries of flows with proper multi-tenancy
- **Modular backend**: Services that can run independently or together

**Current State:**
- Backend: Well-structured Go monolith with clear service boundaries
- Frontend: React + TypeScript, tightly coupled to Tauri APIs
- Storage: File-based JSON storage + keyring for secrets
- Deployment: Tauri desktop app with sidecar Go backend
- **No authentication, multi-tenancy, or team features**

**Target State:**
- Hybrid deployment: Local monolith + Cloud microservices
- Team collaboration with organizations, permissions, sharing
- Platform-agnostic frontend (Tauri + Web)
- Database-backed multi-tenant architecture

---

## PART 1: COMPREHENSIVE ERROR ANALYSIS & EDGE CASES

### 1. Authentication/Authorization Edge Cases

#### **Token Expiration During Active Operations**
**Scenario:** User's JWT token expires while uploading a large flow document or during active AI streaming.

**Current State:** No token refresh mechanism exists.

**Required Implementation:**
```typescript
// frontend/src/api/client.ts
class TokenManager {
  private refreshPromise: Promise<string> | null = null;

  async getValidToken(): Promise<string> {
    const token = this.getToken();
    if (this.isTokenValid(token)) return token;

    // Prevent multiple refresh attempts
    if (this.refreshPromise) return this.refreshPromise;

    this.refreshPromise = this.refreshToken();
    const newToken = await this.refreshPromise;
    this.refreshPromise = null;
    return newToken;
  }

  async refreshToken(): Promise<string> {
    const response = await fetch('/api/auth/refresh', {
      method: 'POST',
      headers: { 'Authorization': `Bearer ${this.getRefreshToken()}` }
    });

    if (!response.ok) {
      // Redirect to login if refresh fails
      this.clearTokens();
      window.location.href = '/login';
      throw new Error('Token refresh failed');
    }

    const { accessToken } = await response.json();
    this.setToken(accessToken);
    return accessToken;
  }
}
```

**Backend Implementation:**
```go
// internal/auth/middleware.go
func TokenRefreshMiddleware(next http.Handler) http.Handler {
  return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    token := extractToken(r)
    claims, err := validateToken(token)
    
    if err != nil || claims.ExpiresAt.Sub(time.Now()) < 5*time.Minute {
      // Token expired or about to expire - attempt refresh
      newToken, err := refreshToken(claims.RefreshToken)
      if err != nil {
        http.Error(w, "Token refresh required", http.StatusUnauthorized)
        return
      }
      
      // Set new token in header
      w.Header().Set("X-New-Token", newToken)
      r.Header.Set("Authorization", "Bearer " + newToken)
    }
    
    next.ServeHTTP(w, r)
  })
}
```

#### **Permission Changes During Active Sessions**
**Scenario:** User's permissions are revoked while they're actively viewing/editing a flow.

**Required Implementation:**
```typescript
// frontend/src/stores/authStore.ts
interface AuthState {
  // ... existing fields
  permissionVersion: number;
  checkPermission: (resource: string, action: string) => boolean;
}

// Permission checking with version validation
const useAuthStore = create<AuthState>((set, get) => ({
  // ... existing implementation
  
  checkPermission: (resource: string, action: string) => {
    const state = get();
    const permissionKey = `${resource}:${action}`;
    
    // Check if user has required role
    const userRoles = state.user?.roles || [];
    return userRoles.some(role => 
      ROLE_PERMISSIONS[role]?.includes(permissionKey)
    );
  },
}));

// Periodic permission validation
useEffect(() => {
  const interval = setInterval(async () => {
    const response = await fetch('/api/auth/permissions-check');
    const { permissionVersion } = await response.json();
    
    if (permissionVersion !== useAuthStore.getState().permissionVersion) {
      // Reload permissions or redirect
      await useAuthStore.getState().refreshPermissions();
    }
  }, 60000); // Check every minute
  
  return () => clearInterval(interval);
}, []);
```

#### **Concurrent Login from Multiple Devices**
**Scenario:** User logs in from multiple devices simultaneously, causing session conflicts.

**Required Implementation:**
```go
// internal/auth/session.go
type SessionManager struct {
  store *redis.Client
}

func (sm *SessionManager) CreateSession(userID string, deviceInfo DeviceInfo) (*Session, error) {
  // Check for existing sessions from same device type
  existingSessions, _ := sm.store.Get("user_sessions:" + userID).Result()
  
  // Optionally: Revise old sessions from same device
  for _, session := range existingSessions {
    if session.DeviceType == deviceInfo.DeviceType {
      sm.RevokeSession(session.ID)
    }
  }
  
  newSession := &Session{
    ID: generateSessionID(),
    UserID: userID,
    DeviceInfo: deviceInfo,
    CreatedAt: time.Now(),
    ExpiresAt: time.Now().Add(24 * time.Hour),
  }
  
  // Store session with expiration
  sessionData, _ := json.Marshal(newSession)
  sm.store.Set("session:"+newSession.ID, sessionData, 24*time.Hour)
  
  return newSession, nil
}

func (sm *SessionManager) RevokeSession(sessionID string) error {
  // Remove from Redis and broadcast revocation event
  sm.store.Del("session:" + sessionID)
  
  // Notify connected clients
  eventBus.Publish("session:revoked", sessionID)
  
  return nil
}
```

#### **Organization Deletion with Active Users**
**Scenario:** Organization is deleted while users are actively working with shared flows.

**Required Implementation:**
```go
// internal/collaboration/org_service.go
func (s *OrgService) DeleteOrg(ctx context.Context, orgID string, requestingUserID string) error {
  // Check if user has permission
  if !s.HasPermission(ctx, requestingUserID, "organization", "delete", orgID) {
    return ErrInsufficientPermissions
  }
  
  // Check for active users
  activeUsers, _ := s.GetActiveUsers(ctx, orgID)
  if len(activeUsers) > 0 {
    // Send force logout notification
    for _, user := range activeUsers {
      eventBus.Publish("org:deleting", OrgDeletionEvent{
        OrgID: orgID,
        UserID: user.ID,
        GracePeriod: 5 * time.Minute,
      })
    }
    
    // Delay deletion or return error with active user list
    return ErrOrgHasActiveUsers
  }
  
  // Proceed with deletion
  return s.db.Transaction(func(tx *sql.Tx) error {
    // Cascade delete flows, permissions, memberships
    if err := s.deleteOrgFlows(tx, orgID); err != nil {
      return err
    }
    
    if err := s.deleteOrgMembers(tx, orgID); err != nil {
      return err
    }
    
    return s.deleteOrg(tx, orgID)
  })
}
```

### 2. Data Migration Failures

#### **Large Flow Document Migrations**
**Scenario:** Migrating flow documents with 10,000+ blocks causes memory issues or timeouts.

**Current State:** No batch processing for large documents.

**Required Implementation:**
```go
// internal/migration/batch_migrator.go
type BatchMigrator struct {
  batchSize    int
  maxRetries   int
  retryDelay   time.Duration
}

func (bm *BatchMigrator) MigrateLargeFlow(ctx context.Context, flow *FlowDocument) error {
  const maxBatchSize = 1000
  
  // Migrate in batches
  for i := 0; i < len(flow.Subflows); i += maxBatchSize {
    end := min(i + maxBatchSize, len(flow.Subflows))
    batch := flow.Subflows[i:end]
    
    err := bm.migrateBatch(ctx, flow.ID, batch)
    if err != nil {
      // Implement retry logic
      for retry := 0; retry < bm.maxRetries; retry++ {
        time.Sleep(bm.retryDelay)
        err = bm.migrateBatch(ctx, flow.ID, batch)
        if err == nil {
          break
        }
      }
      
      if err != nil {
        // Store failed batch for later recovery
        bm.storeFailedBatch(flow.ID, i, end, err)
        return fmt.Errorf("failed to migrate batch %d-%d: %w", i, end, err)
      }
    }
    
    // Update progress
    bm.updateProgress(ctx, flow.ID, i, len(flow.Subflows))
  }
  
  return nil
}

func (bm *BatchMigrator) storeFailedBatch(flowID string, start, end int, err error) {
  // Store failure information for recovery
  failedBatch := FailedBatch{
    FlowID: flowID,
    StartIndex: start,
    EndIndex: end,
    Error: err.Error(),
    Timestamp: time.Now(),
  }
  
  bm.db.Store("migration_failures:"+flowID, failedBatch)
}
```

#### **Corrupted Local Data Handling**
**Scenario:** Local JSON settings files are corrupted or malformed during migration.

**Required Implementation:**
```go
// internal/migration/validator.go
type DataValidator struct {
  schemaManager *SchemaManager
  backupManager *BackupManager
}

func (dv *DataValidator) ValidateAndMigrate(localData []byte) (*MigratedData, error) {
  // Create backup before migration
  backupPath := dv.backupManager.CreateBackup(localData)
  
  // Validate JSON structure
  if !json.Valid(localData) {
    // Attempt repair
    repaired, err := dv.attemptJSONRepair(localData)
    if err != nil {
      return nil, fmt.Errorf("corrupted JSON: %w (backup at: %s)", err, backupPath)
    }
    localData = repaired
  }
  
  // Validate against schema
  if err := dv.schemaManager.Validate(localData); err != nil {
    // Attempt schema migration
    migrated, err := dv.schemaManager.Migrate(localData)
    if err != nil {
      return nil, fmt.Errorf("schema validation failed: %w (backup at: %s)", err, backupPath)
    }
    localData = migrated
  }
  
  // Parse and return migrated data
  var data MigratedData
  if err := json.Unmarshal(localData, &data); err != nil {
    return nil, fmt.Errorf("parse error: %w (backup at: %s)", err, backupPath)
  }
  
  return &data, nil
}

func (dv *DataValidator) attemptJSONRepair(data []byte) ([]byte, error) {
  // Implement JSON repair logic
  var result interface{}
  if err := json.Unmarshal(data, &result); err != nil {
    // Try common fixes
    repairedStr := string(data)
    repairedStr = strings.TrimSpace(repairedStr)
    repairedStr = strings.Trim(repairedStr, ",")
    
    if err := json.Unmarshal([]byte(repairedStr), &result); err != nil {
      return nil, err
    }
  }
  
  return json.Marshal(result)
}
```

#### **Network Failures During Cloud Sync**
**Scenario:** Intermittent network failures during cloud sync cause data inconsistency.

**Required Implementation:**
```typescript
// frontend/src/stores/syncStore.ts
interface SyncState {
  syncQueue: SyncOperation[];
  failedOperations: FailedSyncOperation[];
  isSyncing: boolean;
}

class SyncManager {
  private retryQueue: Map<string, SyncOperation> = new Map();
  private maxRetries = 3;
  private retryDelay = 1000; // Start with 1 second

  async syncWithRetry(operation: SyncOperation): Promise<void> {
    let lastError: Error | null = null;
    
    for (let attempt = 0; attempt < this.maxRetries; attempt++) {
      try {
        await this.executeSync(operation);
        // Success - remove from retry queue
        this.retryQueue.delete(operation.id);
        return;
      } catch (error) {
        lastError = error;
        console.warn(`Sync attempt ${attempt + 1} failed:`, error);
        
        // Exponential backoff
        await this.delay(this.retryDelay * Math.pow(2, attempt));
      }
    }
    
    // All retries failed - store for manual recovery
    this.storeFailedOperation(operation, lastError!);
  }

  private async executeSync(operation: SyncOperation): Promise<void> {
    const response = await fetch('/api/sync', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'Authorization': `Bearer ${await this.getValidToken()}`,
      },
      body: JSON.stringify(operation),
    });

    if (!response.ok) {
      throw new Error(`Sync failed: ${response.statusText}`);
    }

    // Check for conflicts
    const { conflict } = await response.json();
    if (conflict) {
      throw new SyncConflictError('Data conflict detected', conflict);
    }
  }

  private storeFailedOperation(operation: SyncOperation, error: Error): void {
    const failedOp: FailedSyncOperation = {
      ...operation,
      error: error.message,
      timestamp: new Date(),
      retryCount: this.maxRetries,
    };

    // Store in IndexedDB for persistence
    this.storeFailedOperations([failedOp]);
    
    // Notify user
    this.notifyUserOfSyncFailure(failedOp);
  }
}

class SyncConflictError extends Error {
  constructor(message: string, public conflict: ConflictData) {
    super(message);
    this.name = 'SyncConflictError';
  }
}
```

### 3. Multi-tenancy Data Isolation

#### **Cross-Organization Data Leaks**
**Scenario:** User from Organization A accidentally accesses flows from Organization B.

**Required Implementation:**
```go
// internal/auth/isolation.go
type IsolationMiddleware struct {
  db *Database
}

func (im *IsolationMiddleware) RequireOrgAccess(orgID string) Handler {
  return func(w http.ResponseWriter, r *http.Request) {
    user := getUserFromContext(r)
    
    // Verify user has access to organization
    if !im.userHasAccessToOrg(user.ID, orgID) {
      http.Error(w, "Organization access denied", http.StatusForbidden)
      return
    }
    
    // Add org context to request
    ctx := context.WithValue(r.Context(), "org_id", orgID)
    next.ServeHTTP(w, r.WithContext(ctx))
  }
}

func (im *IsolationMiddleware) userHasAccessToOrg(userID, orgID string) bool {
  // Check database for organization membership
  var membership string
  err := im.db.QueryRow(
    "SELECT role FROM organization_members WHERE user_id = ? AND organization_id = ?",
    userID, orgID,
  ).Scan(&membership)
  
  return err == nil && membership != ""
}

// Application-level data isolation
func (im *IsolationMiddleware) FilterFlowsByOrg(ctx context.Context, userID string, flows []FlowDocument) []FlowDocument {
  // Get user's accessible organizations
  orgs, _ := im.getUserOrganizations(ctx, userID)
  orgIDs := make(map[string]bool)
  for _, org := range orgs {
    orgIDs[org.ID] = true
  }
  
  // Filter flows
  var filtered []FlowDocument
  for _, flow := range flows {
    if flow.IsPublic || orgIDs[flow.OrgID] {
      filtered = append(filtered, flow)
    }
  }
  
  return filtered
}
```

#### **Orphaned Records After Deletion**
**Scenario:** User deletion leaves orphaned flow permissions, conversation history, and activity logs.

**Required Implementation:**
```go
// internal/storage/cascade.go
type CascadeManager struct {
  db *Database
}

func (cm *CascadeManager) DeleteUser(ctx context.Context, userID string) error {
  return cm.db.Transaction(func(tx *sql.Tx) error {
    // 1. Transfer or delete user's flows
    if err := cm.handleUserFlows(tx, userID); err != nil {
      return err
    }
    
    // 2. Remove flow permissions
    if _, err := tx.Exec("DELETE FROM flow_permissions WHERE user_id = ?", userID); err != nil {
      return err
    }
    
    // 3. Archive or delete conversations
    if _, err := tx.Exec("UPDATE conversations SET user_id = NULL WHERE user_id = ?", userID); err != nil {
      return err
    }
    
    // 4. Update activity logs
    if _, err := tx.Exec("UPDATE activity_logs SET user_id = NULL WHERE user_id = ?", userID); err != nil {
      return err
    }
    
    // 5. Remove organization memberships
    if _, err := tx.Exec("DELETE FROM organization_members WHERE user_id = ?", userID); err != nil {
      return err
    }
    
    // 6. Delete user preferences
    if _, err := tx.Exec("DELETE FROM user_preferences WHERE user_id = ?", userID); err != nil {
      return err
    }
    
    // 7. Finally delete user
    if _, err := tx.Exec("DELETE FROM users WHERE id = ?", userID); err != nil {
      return err
    }
    
    return nil
  })
}

func (cm *CascadeManager) handleUserFlows(tx *sql.Tx, userID string) error {
  // Get user's flows
  flows, err := cm.getUserFlows(tx, userID)
  if err != nil {
    return err
  }
  
  for _, flow := range flows {
    // Check if flow has other collaborators
    collaborators, _ := cm.getFlowCollaborators(tx, flow.ID, userID)
    
    if len(collaborators) > 0 {
      // Transfer ownership to first collaborator
      if err := cm.transferFlowOwnership(tx, flow.ID, collaborators[0].ID); err != nil {
        return err
      }
    } else {
      // Delete isolated flows
      if err := cm.deleteFlowCascade(tx, flow.ID); err != nil {
        return err
      }
    }
  }
  
  return nil
}
```

### 4. Platform Compatibility Issues

#### **Tauri API Calls in Web Mode**
**Scenario:** Web deployment tries to invoke Tauri APIs causing runtime errors.

**Current Analysis Found:**
- `App.tsx` (520 lines) - direct `invoke()` calls
- `api/client.ts` - `invoke()` for backend config
- `api/flow.ts` - Tauri file dialogs
- `TitleBar.tsx` - window controls

**Required Implementation:**
```typescript
// src/platform/guards.ts
export function isTauri(): boolean {
  return '__TAURI__' in window;
}

export function isWeb(): boolean {
  return !isTauri();
}

// Safe API invocation with fallback
export async function safeInvoke<T>(command: string, args?: any): Promise<T> {
  if (!isTauri()) {
    throw new Error(`Tauri command '${command}' not available in web mode`);
  }
  
  try {
    return await invoke<T>(command, args);
  } catch (error) {
    console.error(`Tauri invoke failed: ${command}`, error);
    throw error;
  }
}

// Component-level platform detection
export function withPlatformDetection<P extends object>(
  Component: React.ComponentType<P>
) {
  return function PlatformDetectedComponent(props: P) {
    const [platform, setPlatform] = useState<'tauri' | 'web'>('unknown');
    const [ready, setReady] = useState(false);

    useEffect(() => {
      setPlatform(isTauri() ? 'tauri' : 'web');
      setReady(true);
    }, []);

    if (!ready) {
      return <div className="loading">Initializing...</div>;
    }

    return <Component {...props} platform={platform} />;
  };
}
```

```typescript
// src/api/client.ts - Platform-agnostic implementation
class BackendClient {
  private config: BackendConfig | null = null;
  private adapter: PlatformAdapter;

  constructor() {
    this.adapter = isTauri() 
      ? new TauriAdapter() 
      : new WebAdapter();
  }

  async initialize(): Promise<void> {
    try {
      this.config = await this.adapter.getBackendConfig();
    } catch (error) {
      console.error('Failed to get backend config:', error);
      throw error;
    }
  }

  async request<T>(
    endpoint: string, 
    options?: RequestInit
  ): Promise<T> {
    if (!this.config) {
      await this.initialize();
    }

    const url = `${this.config!.apiUrl}${endpoint}`;
    
    // Add authentication headers
    const headers = {
      ...options?.headers,
      'Authorization': `Bearer ${this.config!.token}`,
    };

    const response = await fetch(url, {
      ...options,
      headers,
    });

    if (!response.ok) {
      throw new Error(`HTTP ${response.status}: ${response.statusText}`);
    }

    return response.json();
  }
}
```

### 5. Real-time Collaboration Failures

#### **WebSocket Disconnection During Editing**
**Scenario:** User loses connection while actively editing a flow, causing data loss.

**Required Implementation:**
```typescript
// src/services/collaboration.ts
class CollaborationService {
  private ws: WebSocket | null = null;
  private reconnectAttempts = 0;
  private maxReconnectAttempts = 5;
  private pendingOperations: Map<string, FlowOperation> = new Map();
  private operationAckTimeout = 5000;

  connect(flowId: string) {
    const wsUrl = `ws://localhost/api/flows/${flowId}/collaborate`;
    this.ws = new WebSocket(wsUrl);

    this.ws.onopen = () => {
      console.log('Collaboration connected');
      this.reconnectAttempts = 0;
      this.sendPendingOperations();
    };

    this.ws.onclose = (event) => {
      console.warn('Collaboration disconnected:', event.code, event.reason);
      this.handleDisconnection();
    };

    this.ws.onmessage = (event) => {
      this.handleMessage(JSON.parse(event.data));
    };

    this.ws.onerror = (error) => {
      console.error('WebSocket error:', error);
    };
  }

  private handleDisconnection() {
    if (this.reconnectAttempts < this.maxReconnectAttempts) {
      const delay = Math.min(1000 * Math.pow(2, this.reconnectAttempts), 30000);
      
      setTimeout(() => {
        this.reconnectAttempts++;
        console.log(`Reconnection attempt ${this.reconnectAttempts}`);
        this.connect(this.currentFlowId);
      }, delay);
    } else {
      // Max reconnection attempts reached - notify user
      this.notifyUserOfDisconnection();
      this.enableOfflineMode();
    }
  }

  async sendOperation(operation: FlowOperation) {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) {
      // Queue operation for later
      this.pendingOperations.set(operation.id, operation);
      this.notifyUserOfQueueing();
      return;
    }

    this.ws.send(JSON.stringify(operation));

    // Wait for acknowledgment
    const acked = await this.waitForAck(operation.id);
    if (!acked) {
      // Resend or queue
      this.pendingOperations.set(operation.id, operation);
    }
  }

  private async waitForAck(operationId: string): Promise<boolean> {
    return new Promise((resolve) => {
      const timeout = setTimeout(() => resolve(false), this.operationAckTimeout);
      
      const listener = (data: any) => {
        if (data.type === 'ack' && data.operationId === operationId) {
          clearTimeout(timeout);
          this.removeEventListener('ack', listener);
          resolve(true);
        }
      };
      
      this.addEventListener('ack', listener);
    });
  }

  private enableOfflineMode() {
    // Enable local editing mode
    this.isOffline = true;
    
    // Save to IndexedDB
    this.saveLocalState();
    
    // Notify user of offline mode
    this.notifyUserOfOfflineMode();
  }
}
```

### 6. Cloud Deployment Issues

#### **Database Connection Pool Exhaustion**
**Scenario:** High traffic causes database connection pool exhaustion.

**Required Implementation:**
```go
// internal/storage/pool.go
type ConnectionPoolManager struct {
  pools map[string]*sql.DB
  metrics *PoolMetrics
}

func NewConnectionPoolManager(config *DatabaseConfig) (*ConnectionPoolManager, error) {
  db, err := sql.Open("postgres", config.ConnectionString)
  if err != nil {
    return nil, err
  }

  // Configure connection pool
  db.SetMaxOpenConns(config.MaxOpenConns) // e.g., 25
  db.SetMaxIdleConns(config.MaxIdleConns) // e.g., 5
  db.SetConnMaxLifetime(time.Hour)
  db.SetConnMaxIdleTime(time.Minute * 5)

  manager := &ConnectionPoolManager{
    pools: map[string]*sql.DB{"default": db},
    metrics: &PoolMetrics{},
  }

  // Start monitoring
  go manager.monitorPool()

  return manager, nil
}

func (pm *ConnectionPoolManager) monitorPool() {
  ticker := time.NewTicker(30 * time.Second)
  defer ticker.Stop()

  for range ticker.C {
    for name, db := range pm.pools {
      stats := db.Stats()
      
      pm.metrics.Record(name, PoolStats{
        OpenConnections: stats.OpenConnections,
        InUse: stats.InUse,
        Idle: stats.Idle,
        WaitCount: stats.WaitCount,
        WaitDuration: stats.WaitDuration,
      })

      // Alert if pool is exhausted
      if stats.InUse >= stats.MaxOpenConnections * 0.9 {
        pm.alertPoolExhaustion(name, stats)
      }
    }
  }
}

func (pm *ConnectionPoolManager) alertPoolExhaustion(name string, stats sql.DBStats) {
  // Send alert to monitoring system
  alerting.SendAlert(&Alert{
    Severity: "warning",
    Title: "Database Pool Near Exhaustion",
    Message: fmt.Sprintf("Pool %s: %d/%d connections in use", 
      name, stats.InUse, stats.MaxOpenConnections),
    Metrics: stats,
  })
}
```

### 7. Performance Degradation

#### **Large Flow Document Loading**
**Scenario:** Loading flow documents with 10,000+ blocks causes UI freezing.

**Current Analysis Found:**
- `flowStore.ts` is 457 lines with synchronous state updates
- No pagination or virtualization in BlockView component

**Required Implementation:**
```typescript
// src/services/flowLoader.ts
class FlowLoader {
  private cache = new Map<string, FlowDocument>();
  private loadingPromises = new Map<string, Promise<FlowDocument>>();

  async loadFlow(flowId: string, options?: LoadOptions): Promise<FlowDocument> {
    // Check cache first
    if (this.cache.has(flowId)) {
      return this.cache.get(flowId)!;
    }

    // Check if already loading
    if (this.loadingPromises.has(flowId)) {
      return this.loadingPromises.get(flowId)!;
    }

    // Start loading
    const loadingPromise = this.loadFlowFromAPI(flowId, options);
    this.loadingPromises.set(flowId, loadingPromise);

    try {
      const flow = await loadingPromise;
      this.cache.set(flowId, flow);
      return flow;
    } finally {
      this.loadingPromises.delete(flowId);
    }
  }

  private async loadFlowFromAPI(flowId: string, options?: LoadOptions): Promise<FlowDocument> {
    const { batchSize = 100, loadMetadata = true } = options || {};

    // Load metadata first
    if (loadMetadata) {
      const metadata = await this.client.request<{ metadata: FlowMetadata }>(
        `/api/flows/${flowId}/metadata`
      );

      // Return partial document for initial render
      return {
        id: flowId,
        metadata: metadata.metadata,
        subflows: [], // Will be loaded progressively
        isLoading: true,
      } as FlowDocument;
    }

    // Load subflows in batches
    const subflows = await this.loadSubflowsBatch(flowId, 0, batchSize);
    
    return {
      id: flowId,
      subflows,
      // ... other fields
    };
  }

  private async loadSubflowsBatch(
    flowId: string, 
    offset: number, 
    limit: number
  ): Promise<Subflow[]> {
    const response = await this.client.request<{ subflows: Subflow[] }>(
      `/api/flows/${flowId}/subflows?offset=${offset}&limit=${limit}`
    );
    return response.subflows;
  }
}
```

---

## PART 2: TEST COVERAGE ANALYSIS & MIGRATION

### Current Test Coverage Analysis

#### **Backend Testing (Go)**
**Current State:**
- **76 source files** with only **39 test files** (51% coverage)
- **Good coverage**: AI providers, parser (with golden files), storage operations
- **Critical gaps**: 
  - **ZERO tests** for API handlers (`internal/api/handlers.go` - 743 lines, 40+ endpoints)
  - **ZERO integration tests** (all unit tests only)
  - **Minimal streaming/concurrency testing**

**Files Requiring Immediate Tests:**
```go
// CRITICAL: No tests exist for these files
internal/api/handlers.go          // 743 lines, 40+ endpoints
internal/manager/manager.go        // Core orchestration
internal/service/chat.go          // Streaming functionality
internal/service/flow.go          // Complex business logic
internal/api/router.go            // Route configuration

// HIGH PRIORITY: Limited test coverage
internal/ai/provider.go           // Core AI abstraction
internal/ai/factory.go            // Provider instantiation
internal/analyzer/engine.go       // Analysis rules
```

#### **Frontend Testing (TypeScript/React)**
**Current State:**
- **40+ components** with only **4 test files**
- **Existing tests**: utilities (`lib/`), stores (`chatStore`, `flowStore`)
- **Critical gaps**:
  - **ZERO component tests** (React Testing Library configured but unused)
  - **ZERO API client tests** 
  - **ZERO E2E tests**
  - **ZERO integration tests**

**Components Requiring Immediate Tests:**
```typescript
// CRITICAL: No tests exist for these components
src/App.tsx                       // 520 lines, main app logic
src/components/flow/BlockView.tsx // 220 lines, complex rendering
src/components/chat/AITab.tsx    // 256 lines, chat interface
src/components/sidebar/Sidebar.tsx // 277 lines, file navigation

// HIGH PRIORITY: Business logic components
src/components/chat/hooks/useAIChat.ts // Streaming logic
src/stores/flowStore.ts          // 457 lines, complex state
src/api/client.ts                // Core API communication
```

### Test Infrastructure Requirements

#### **Backend Test Framework Enhancement**
**Required Setup:**
```go
// internal/testing/testsuite.go
type TestSuite struct {
    db          *sql.DB
    redis       *redis.Client
    router      *Router
    authManager *AuthManager
    cleanup     func()
}

func NewTestSuite(t *testing.T) *TestSuite {
    // Setup test database
    db := setupTestDB(t)
    
    // Setup test Redis
    redis := setupTestRedis(t)
    
    // Create test router
    router := NewTestRouter(db, redis)
    
    // Create auth manager for tests
    authManager := NewTestAuthManager(db)
    
    return &TestSuite{
        db: db,
        redis: redis,
        router: router,
        authManager: authManager,
        cleanup: func() {
            db.Close()
            redis.Close()
        },
    }
}

// Helper functions for testing
func (ts *TestSuite) CreateUser(t *testing.T, email string) *User {
    user, err := ts.authManager.Register(context.Background(), email, "password")
    require.NoError(t, err)
    return user
}

func (ts *TestSuite) CreateFlow(t *testing.T, userID string) *FlowDocument {
    flow := &FlowDocument{
        ID: generateID(),
        Name: "Test Flow",
        OwnerID: userID,
        // ... other fields
    }
    
    err := ts.db.QueryRow(
        "INSERT INTO flow_documents (id, name, owner_id) VALUES ($1, $2, $3)",
        flow.ID, flow.Name, flow.OwnerID,
    ).Err()
    require.NoError(t, err)
    
    return flow
}

func (ts *TestSuite) MakeAuthenticatedRequest(t *testing.T, user *User, method, path string, body interface{}) *httptest.ResponseRecorder {
    token, _ := ts.authManager.GenerateToken(user.ID)
    
    var bodyReader io.Reader
    if body != nil {
        bodyBytes, _ := json.Marshal(body)
        bodyReader = bytes.NewBuffer(bodyBytes)
    }
    
    req := httptest.NewRequest(method, path, bodyReader)
    req.Header.Set("Authorization", "Bearer "+token)
    req.Header.Set("Content-Type", "application/json")
    
    rr := httptest.NewRecorder()
    ts.router.ServeHTTP(rr, req)
    
    return rr
}
```

#### **API Handler Tests (CRITICAL)**
**New Test File: `internal/api/handlers_test.go`**
```go
func TestFlowHandlers_CreateFlow(t *testing.T) {
    ts := NewTestSuite(t)
    defer ts.cleanup()
    
    user := ts.CreateUser(t, "test@example.com")
    
    t.Run("successful flow creation", func(t *testing.T) {
        payload := map[string]interface{}{
            "name": "Test Flow",
            "description": "A test flow",
            "content": map[string]interface{}{
                "subflows": []interface{}{},
            },
        }
        
        rr := ts.MakeAuthenticatedRequest(t, user, "POST", "/api/flows", payload)
        
        assert.Equal(t, http.StatusCreated, rr.Code)
        
        var response map[string]interface{}
        json.Unmarshal(rr.Body.Bytes(), &response)
        
        assert.NotEmpty(t, response["id"])
        assert.Equal(t, "Test Flow", response["name"])
    })
    
    t.Run("unauthorized flow creation", func(t *testing.T) {
        req := httptest.NewRequest("POST", "/api/flows", bytes.NewBufferString("{}"))
        rr := httptest.NewRecorder()
        ts.router.ServeHTTP(rr, req)
        
        assert.Equal(t, http.StatusUnauthorized, rr.Code)
    })
    
    t.Run("invalid payload", func(t *testing.T) {
        payload := map[string]interface{}{
            "name": "", // Invalid: empty name
        }
        
        rr := ts.MakeAuthenticatedRequest(t, user, "POST", "/api/flows", payload)
        
        assert.Equal(t, http.StatusBadRequest, rr.Code)
    })
}

func TestFlowHandlers_GetFlow(t *testing.T) {
    ts := NewTestSuite(t)
    defer ts.cleanup()
    
    user := ts.CreateUser(t, "test@example.com")
    flow := ts.CreateFlow(t, user.ID)
    
    t.Run("get own flow", func(t *testing.T) {
        rr := ts.MakeAuthenticatedRequest(t, user, "GET", "/api/flows/"+flow.ID, nil)
        
        assert.Equal(t, http.StatusOK, rr.Code)
        
        var response map[string]interface{}
        json.Unmarshal(rr.Body.Bytes(), &response)
        
        assert.Equal(t, flow.ID, response["id"])
        assert.Equal(t, flow.Name, response["name"])
    })
    
    t.Run("get non-existent flow", func(t *testing.T) {
        rr := ts.MakeAuthenticatedRequest(t, user, "GET", "/api/flows/nonexistent", nil)
        
        assert.Equal(t, http.StatusNotFound, rr.Code)
    })
    
    t.Run("get another user's private flow", func(t *testing.T) {
        otherUser := ts.CreateUser(t, "other@example.com")
        privateFlow := ts.CreateFlow(t, otherUser.ID)
        
        rr := ts.MakeAuthenticatedRequest(t, user, "GET", "/api/flows/"+privateFlow.ID, nil)
        
        assert.Equal(t, http.StatusForbidden, rr.Code)
    })
}
```

#### **Integration Test Framework**
**New Test File: `internal/testing/integration/integration_test.go`**
```go
func TestIntegration_FlowCreationAndAnalysis(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping integration test in short mode")
    }
    
    ts := NewTestSuite(t)
    defer ts.cleanup()
    
    // Create test organization
    org := &Organization{
        ID: generateID(),
        Name: "Test Organization",
        OwnerID: ts.CreateUser(t, "owner@example.com").ID,
    }
    
    // Create flow
    flow := &FlowDocument{
        ID: generateID(),
        Name: "Integration Test Flow",
        OrgID: org.ID,
        Content: generateTestFlowContent(),
    }
    
    // Test complete workflow
    t.Run("create flow", func(t *testing.T) {
        rr := ts.MakeAuthenticatedRequest(t, org.Owner, "POST", "/api/flows", flow)
        assert.Equal(t, http.StatusCreated, rr.Code)
    })
    
    t.Run("analyze flow", func(t *testing.T) {
        rr := ts.MakeAuthenticatedRequest(t, org.Owner, "POST", "/api/flows/"+flow.ID+"/analyze", nil)
        assert.Equal(t, http.StatusOK, rr.Code)
        
        var response map[string]interface{}
        json.Unmarshal(rr.Body.Bytes(), &response)
        
        assert.NotEmpty(t, response["findings"])
    })
    
    t.Run("share flow with team member", func(t *testing.T) {
        member := ts.CreateUser(t, "member@example.com")
        
        sharePayload := map[string]interface{}{
            "userId": member.ID,
            "permission": "write",
        }
        
        rr := ts.MakeAuthenticatedRequest(t, org.Owner, "POST", "/api/flows/"+flow.ID+"/share", sharePayload)
        assert.Equal(t, http.StatusOK, rr.Code)
    })
    
    t.Run("member can access shared flow", func(t *testing.T) {
        rr := ts.MakeAuthenticatedRequest(t, member, "GET", "/api/flows/"+flow.ID, nil)
        assert.Equal(t, http.StatusOK, rr.Code)
    })
}
```

#### **Frontend Test Infrastructure**
**Required Setup:**
```typescript
// src/testing/testHelpers.tsx
import { render, screen, waitFor } from '@testing-library/react'
import { BrowserRouter } from 'react-router-dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

// Mock stores
jest.mock('@/stores/flowStore')
jest.mock('@/stores/chatStore')
jest.mock('@/stores/settingsStore')

// Test providers
function TestProviders({ children }: { children: React.ReactNode }) {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  })

  return (
    <BrowserRouter>
      <QueryClientProvider client={queryClient}>
        {children}
      </QueryClientProvider>
    </BrowserRouter>
  )
}

// Custom render function
function renderWithProviders(ui: React.ReactElement) {
  return render(<TestProviders>{ui}</TestProviders>)
}

// Mock API client
const mockApiClient = {
  get: jest.fn(),
  post: jest.fn(),
  put: jest.fn(),
  delete: jest.fn(),
}

jest.mock('@/api/client', () => ({
  apiClient: mockApiClient,
}))

export { renderWithProviders, screen, waitFor, mockApiClient }
```

#### **Frontend Component Tests**
**New Test File: `src/components/flow/BlockView.test.tsx`**
```typescript
describe('BlockView Component', () => {
  const mockBlock = {
    id: 'block-1',
    name: 'Test Block',
    type: 'ACTION',
    properties: {},
    children: [],
  }

  beforeEach(() => {
    jest.clearAllMocks()
  })

  test('renders block correctly', () => {
    renderWithProviders(<BlockView block={mockBlock} />)
    
    expect(screen.getByText('Test Block')).toBeInTheDocument()
    expect(screen.getByText('ACTION')).toBeInTheDocument()
  })

  test('handles block selection', async () => {
    const mockOnSelect = jest.fn()
    renderWithProviders(<BlockView block={mockBlock} onSelect={mockOnSelect} />)
    
    const blockElement = screen.getByTestId('block-block-1')
    fireEvent.click(blockElement)
    
    await waitFor(() => {
      expect(mockOnSelect).toHaveBeenCalledWith('block-1')
    })
  })

  test('displays block properties', () => {
    const blockWithProps = {
      ...mockBlock,
      properties: { 'action': 'SendEmail', 'to': 'test@example.com' },
    }
    
    renderWithProviders(<BlockView block={blockWithProps} />)
    
    expect(screen.getByText('SendEmail')).toBeInTheDocument()
    expect(screen.getByText('test@example.com')).toBeInTheDocument()
  })

  test('handles nested blocks', () => {
    const nestedBlock = {
      ...mockBlock,
      children: [
        { id: 'child-1', name: 'Child Block', type: 'COMMENT', children: [] },
      ],
    }
    
    renderWithProviders(<BlockView block={nestedBlock} />)
    
    expect(screen.getByText('Child Block')).toBeInTheDocument()
  })

  test('shows loading state', () => {
    renderWithProviders(<BlockView block={mockBlock} isLoading={true} />)
    
    expect(screen.getByTestId('loading-spinner')).toBeInTheDocument()
  })
})
```

**New Test File: `src/components/chat/AITab.test.tsx`**
```typescript
describe('AITab Component', () => {
  const mockFlow = {
    id: 'flow-1',
    name: 'Test Flow',
    subflows: [],
  }

  beforeEach(() => {
    jest.clearAllMocks()
    // Mock auth store
    useAuthStore.mockReturnValue({
      user: { id: 'user-1', email: 'test@example.com' },
      isAuthenticated: true,
    })
  })

  test('renders chat input when flow is loaded', () => {
    renderWithProviders(<AITab flow={mockFlow} />)
    
    expect(screen.getByPlaceholderText(/ask about your flow/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /send message/i })).toBeInTheDocument()
  })

  test('shows authentication error when not authenticated', () => {
    useAuthStore.mockReturnValue({
      user: null,
      isAuthenticated: false,
    })
    
    renderWithProviders(<AITab flow={mockFlow} />)
    
    expect(screen.getByText(/please sign in/i)).toBeInTheDocument()
  })

  test('sends message and displays response', async () => {
    const mockResponse = { content: 'Test response' }
    mockApiClient.post.mockResolvedValue({ data: mockResponse })
    
    renderWithProviders(<AITab flow={mockFlow} />)
    
    const input = screen.getByPlaceholderText(/ask about your flow/i)
    const sendButton = screen.getByRole('button', { name: /send message/i })
    
    fireEvent.change(input, { target: { value: 'Test question' } })
    fireEvent.click(sendButton)
    
    await waitFor(() => {
      expect(mockApiClient.post).toHaveBeenCalledWith('/api/chat/message', {
        flowId: 'flow-1',
        message: 'Test question',
      })
      expect(screen.getByText('Test response')).toBeInTheDocument()
    })
  })

  test('handles streaming responses', async () => {
    let streamCallback: (data: string) => void
    mockApiClient.post.mockImplementation((url, options) => {
      return {
        stream: (callback: (data: string) => void) => {
          streamCallback = callback
        },
      }
    })
    
    renderWithProviders(<AITab flow={mockFlow} />)
    
    const input = screen.getByPlaceholderText(/ask about your flow/i)
    fireEvent.change(input, { target: { value: 'Streaming question' } })
    fireEvent.click(screen.getByRole('button', { name: /send message/i }))
    
    // Simulate streaming response
    streamCallback!('Hello ')
    streamCallback!('World')
    
    await waitFor(() => {
      expect(screen.getByText('Hello World')).toBeInTheDocument()
    })
  })
})
```

### Test Migration Strategy

#### **Tests to Remove (0% - Obsolete)**
```bash
# These tests will become obsolete after refactor:
frontend/src/stores/settingsStore.test.ts  # Will be replaced by auth/cloud tests
internal/storage/keyring_test.go           # Platform-specific, needs rewrite
internal/api/handlers_test.go              # Current implementation, complete rewrite needed
```

#### **Tests to Rewrite (100% - Major Changes)**
```bash
# Backend tests requiring complete rewrite:
internal/service/flow_test.go              # New multi-tenancy logic
internal/service/chat_test.go              # Streaming + auth changes  
internal/analyzer/engine_test.go           # Permission-aware analysis
internal/storage/settings_test.go          # Database-backed storage

# Frontend tests requiring complete rewrite:
frontend/src/stores/flowStore.test.ts      # Platform-agnostic state
frontend/src/stores/chatStore.test.ts      # Streaming + error handling
frontend/src/api/client.test.ts            # Platform adapter + retry
```

#### **Tests to Update (30% - Minor Changes)**
```bash
# Tests requiring minor updates:
internal/parser/parser_test.go             # Add error cases
internal/ai/factory_test.go                # New provider tests
internal/ai/openai_test.go                 # Add retry/cancellation tests
frontend/src/lib/utils.test.ts             # Add platform tests
```

#### **New Tests Required (CRITICAL)**
```bash
# Backend tests (NEW):
internal/api/handlers_test.go              # 40+ endpoint tests
internal/auth/middleware_test.go           # Auth middleware tests
internal/collaboration/org_service_test.go # Organization tests
internal/collaboration/permissions_test.go # Permission tests
internal/websocket/hub_test.go             # WebSocket tests
internal/migration/migrator_test.go        # Migration tests

# Frontend tests (NEW):
src/components/auth/LoginForm.test.tsx     # Auth component tests
src/components/library/LibraryBrowser.test.tsx  # Library tests
src/components/sharing/ShareDialog.test.tsx     # Sharing tests
src/stores/authStore.test.ts               # Auth state tests
src/stores/syncStore.test.ts               # Sync state tests
src/services/collaboration.test.ts         # Collaboration service tests
```

---

## PART 3: FRONTEND REORGANIZATION & STRUCTURE

### Current Frontend Organization Analysis

#### **Component Organization (Quality: 6/10)**
**Current Structure:**
```
src/components/
├── chat/          # Chat components (good organization)
├── flow/          # Flow visualization (mixed concerns)
├── graph/         # Graph views (good)
├── inspector/     # Inspector panels (oversized)
├── settings/      # Settings UI (good)
├── sidebar/       # File navigation (oversized)
├── layout/        # Layout components (good)
└── shared/        # Shared components (good)
```

**Issues Identified:**
1. **Oversized components**: 
   - `App.tsx` (520 lines) - Mixed platform detection + routing
   - `AITab.tsx` (256 lines) - Business logic in UI
   - `Sidebar.tsx` (277 lines) - File operations + UI
   - `BlockView.tsx` (220 lines) - Complex rendering logic

2. **Tauri-tight coupling**:
   - Direct `invoke()` calls in multiple components
   - No platform abstraction
   - Hardcoded localhost assumptions

3. **Business logic in UI**:
   - API calls in hooks without service layer
   - State management mixed with rendering
   - No separation of concerns

#### **New Frontend Structure Proposal**

```
src/
├── platform/                    # Platform abstraction layer
│   ├── adapters/
│   │   ├── TauriAdapter.ts     # Tauri-specific implementations
│   │   ├── WebAdapter.ts       # Web-specific implementations
│   │   └── index.ts            # Adapter factory
│   ├── guards.ts               # Platform detection utilities
│   └── types.ts                # Platform-specific types
├── services/                   # Business logic layer
│   ├── api/
│   │   ├── ApiClient.ts        # Generic API client
│   │   ├── FlowApi.ts          # Flow-specific API
│   │   ├── ChatApi.ts          # Chat-specific API
│   │   └── AuthApi.ts          # Auth-specific API
│   ├── collaboration/
│   │   ├── CollaborationService.ts  # Real-time collaboration
│   │   └── PermissionService.ts     # Permission checking
│   ├── sync/
│   │   ├── SyncManager.ts      # Sync orchestration
│   │   └── ConflictResolver.ts # Conflict resolution
│   └── storage/
│       ├── IndexedDBStorage.ts # Browser storage
│       └── CacheManager.ts     # Response caching
├── stores/                     # State management
│   ├── domain/                 # Domain state
│   │   ├── flowStore.ts        # Flow document state
│   │   ├── chatStore.ts        # Chat conversation state
│   │   ├── authStore.ts        # Authentication state
│   │   └── orgStore.ts         # Organization state
│   └── ui/                     # UI state
│       ├── uiStore.ts          # UI preferences
│       └── settingsStore.ts    # User settings
├── components/                 # React components
│   ├── auth/                   # Authentication components
│   │   ├── LoginForm.tsx
│   │   ├── RegisterForm.tsx
│   │   └── ProtectedRoute.tsx
│   ├── library/                # Library browsing
│   │   ├── LibraryBrowser.tsx
│   │   ├── FlowCard.tsx
│   │   └── LibraryNav.tsx
│   ├── sharing/                # Flow sharing
│   │   ├── ShareDialog.tsx
│   │   ├── PermissionSelect.tsx
│   │   └── CollaboratorList.tsx
│   ├── collaboration/          # Real-time collaboration
│   │   ├── PresenceIndicators.tsx
│   │   ├── CollaborativeCursors.tsx
│   │   └── ConflictResolution.tsx
│   ├── flow/                   # Flow visualization
│   │   ├── containers/         # Container components
│   │   │   ├── FlowViewContainer.tsx
│   │   │   └── BlockViewContainer.tsx
│   │   └── views/              # View components
│   │       ├── FlowView.tsx
│   │       ├── BlockView.tsx
│   │       └── SubflowView.tsx
│   ├── chat/                   # Chat interface
│   │   ├── containers/         # Container components
│   │   │   ├── AITabContainer.tsx
│   │   │   └── ChatInputContainer.tsx
│   │   └── views/              # View components
│   │       ├── AITab.tsx
│   │       ├── ChatInput.tsx
│   │       └── MessageList.tsx
│   ├── layout/                 # Layout components
│   └── shared/                 # Shared components
├── hooks/                      # Custom React hooks
│   ├── usePlatform.ts          # Platform detection
│   ├── useAuth.ts              # Authentication state
│   ├── usePermissions.ts       # Permission checking
│   ├── useCollaboration.ts     # Real-time collaboration
│   └── useSync.ts              # Sync management
├── types/                      # TypeScript types
│   ├── domain/                 # Domain types
│   │   ├── flow.ts             # Flow document types
│   │   ├── chat.ts             # Chat types
│   │   └── organization.ts     # Organization types
│   ├── api/                    # API types
│   │   ├── requests.ts         # Request types
│   │   ├── responses.ts        # Response types
│   └── ui/                     # UI types
│       ├── components.ts       # Component prop types
│       └── layouts.ts          # Layout types
├── utils/                      # Utility functions
│   ├── validation.ts           # Input validation
│   ├── formatting.ts           # Data formatting
│   └── errorHandling.ts        # Error handling utilities
└── App.tsx                     # Application root
```

### Component Reorganization Details

#### **Splitting Oversized Components**

**1. App.tsx (520 lines → split into 4 components)**
```typescript
// Current: App.tsx (520 lines)
// Split into:
// - src/App.tsx (100 lines) - Root component + routing
// - src/platform/PlatformInitializer.tsx (80 lines) - Platform detection
// - src/components/auth/AuthProvider.tsx (120 lines) - Auth context
// - src/components/layout/AppLayout.tsx (220 lines) - Main layout

// src/App.tsx (SIMPLIFIED)
function App() {
  return (
    <PlatformInitializer>
      <AuthProvider>
        <AppLayout>
          <Routes>
            <Route path="/" element={<FlowExplorer />} />
            <Route path="/login" element={<LoginForm />} />
            <Route path="/libraries" element={<LibraryBrowser />} />
          </Routes>
        </AppLayout>
      </AuthProvider>
    </PlatformInitializer>
  );
}

// src/platform/PlatformInitializer.tsx (NEW)
function PlatformInitializer({ children }: { children: React.ReactNode }) {
  const [platform, setPlatform] = useState<'tauri' | 'web'>('unknown');
  const [ready, setReady] = useState(false);

  useEffect(() => {
    const detectPlatform = async () => {
      const detectedPlatform = isTauri() ? 'tauri' : 'web';
      setPlatform(detectedPlatform);
      
      // Initialize platform-specific services
      await initializePlatformServices(detectedPlatform);
      
      setReady(true);
    };

    detectPlatform();
  }, []);

  if (!ready) {
    return <LoadingScreen />;
  }

  return (
    <PlatformContext.Provider value={{ platform }}>
      {children}
    </PlatformContext.Provider>
  );
}
```

**2. AITab.tsx (256 lines → split into 3 components)**
```typescript
// Current: src/components/chat/AITab.tsx (256 lines)
// Split into:
// - src/components/chat/views/AITab.tsx (120 lines) - Main UI
// - src/components/chat/containers/AITabContainer.tsx (80 lines) - Business logic
// - src/services/chat/ChatService.ts (100 lines) - API calls

// src/components/chat/containers/AITabContainer.tsx (NEW)
function AITabContainer() {
  const { flow, selectedBlock } = useFlowContext();
  const { user } = useAuthContext();
  const chatService = useChatService();

  const handleSendMessage = async (message: string, excludeContext: boolean) => {
    if (!user) {
      throw new Error('Authentication required');
    }

    try {
      await chatService.sendMessage({
        flowId: flow.id,
        message,
        contextBlockId: excludeContext ? undefined : selectedBlock?.id,
      });
    } catch (error) {
      console.error('Failed to send message:', error);
      // Error handling
    }
  };

  const handlePreviewContext = async (message: string) => {
    return chatService.previewContext({
      flowId: flow.id,
      message,
      contextBlockId: selectedBlock?.id,
    });
  };

  return (
    <AITabView
      flow={flow}
      selectedBlock={selectedBlock}
      onSendMessage={handleSendMessage}
      onPreviewContext={handlePreviewContext}
    />
  );
}

// src/components/chat/views/AITab.tsx (SIMPLIFIED)
function AITabView({ flow, selectedBlock, onSendMessage, onPreviewContext }) {
  // Only UI logic, no business logic
  const [messageInput, setMessageInput] = useState('');
  const [isPreviewing, setIsPreviewing] = useState(false);

  const handleSend = () => {
    onSendMessage(messageInput, false);
    setMessageInput('');
  };

  // UI rendering only
  return (
    <div className="ai-tab">
      <ConnectionPanel />
      <ChatInput
        value={messageInput}
        onChange={setMessageInput}
        onSend={handleSend}
        onPreview={() => setIsPreviewing(true)}
      />
    </div>
  );
}
```

**3. Sidebar.tsx (277 lines → split into 3 components)**
```typescript
// Current: src/components/sidebar/Sidebar.tsx (277 lines)
// Split into:
// - src/components/sidebar/views/Sidebar.tsx (120 lines) - UI layout
// - src/components/sidebar/containers/SidebarContainer.tsx (100 lines) - File operations
// - src/services/file/FileService.ts (80 lines) - File API calls

// src/components/sidebar/containers/SidebarContainer.tsx (NEW)
function SidebarContainer() {
  const { platform } = usePlatformContext();
  const fileService = useFileService();
  const [recentFiles, setRecentFiles] = useState<RecentFile[]>([]);

  useEffect(() => {
    loadRecentFiles();
  }, []);

  const loadRecentFiles = async () => {
    try {
      const files = await fileService.getRecentFiles();
      setRecentFiles(files);
    } catch (error) {
      console.error('Failed to load recent files:', error);
    }
  };

  const handleFileOpen = async () => {
    try {
      const filePath = await fileService.openFile();
      if (filePath) {
        // Load flow
        await loadFlow(filePath);
      }
    } catch (error) {
      console.error('Failed to open file:', error);
    }
  };

  const handleFileSelect = (fileId: string) => {
    // File selection logic
  };

  return (
    <SidebarView
      recentFiles={recentFiles}
      platform={platform}
      onFileOpen={handleFileOpen}
      onFileSelect={handleFileSelect}
    />
  );
}
```

### State Management Reorganization

#### **Current State Issues:**
```typescript
// Current: src/stores/flowStore.ts (457 lines)
// Issues:
// - Mixed domain and UI state
// - Direct API calls in store
// - No error handling
// - Platform-specific code mixed in

// Current: src/stores/settingsStore.ts (214 lines)
// Issues:
// - Settings persistence mixed with state
// - No validation
// - Platform-specific assumptions
```

#### **New State Management Structure:**

```typescript
// src/stores/domain/flowStore.ts (DOMAIN STATE ONLY)
interface FlowState {
  // Domain state only - no UI state
  currentFlow: FlowDocument | null;
  selectedBlockId: string | null;
  parseProgress: number;
  error: Error | null;

  // Actions
  loadFlow: (flowId: string) => Promise<void>;
  selectBlock: (blockId: string) => void;
  updateBlock: (blockId: string, updates: Partial<Block>) => void;
}

const useFlowStore = create<FlowState>((set, get) => ({
  currentFlow: null,
  selectedBlockId: null,
  parseProgress: 0,
  error: null,

  loadFlow: async (flowId: string) => {
    set({ parseProgress: 0, error: null });
    
    try {
      const flowService = new FlowService();
      const flow = await flowService.loadFlow(flowId);
      
      set({ 
        currentFlow: flow, 
        parseProgress: 100,
        error: null 
      });
    } catch (error) {
      set({ 
        error: error as Error,
        parseProgress: 0
      });
    }
  },

  selectBlock: (blockId: string) => {
    set({ selectedBlockId: blockId });
  },

  updateBlock: (blockId: string, updates: Partial<Block>) => {
    const { currentFlow } = get();
    if (!currentFlow) return;

    const updatedFlow = updateFlowBlock(currentFlow, blockId, updates);
    set({ currentFlow: updatedFlow });
  },
}));

// src/stores/ui/uiStore.ts (UI STATE ONLY)
interface UIState {
  // UI preferences only
  sidebarCollapsed: boolean;
  inspectorPanel: 'details' | 'ai' | 'findings';
  theme: Theme;
  density: 'comfortable' | 'compact';

  // Actions
  toggleSidebar: () => void;
  setInspectorPanel: (panel: 'details' | 'ai' | 'findings') => void;
  setTheme: (theme: Theme) => void;
}

const useUIStore = create<UIState>((set) => ({
  sidebarCollapsed: false,
  inspectorPanel: 'details',
  theme: 'dark',
  density: 'comfortable',

  toggleSidebar: () => {
    set((state) => ({ sidebarCollapsed: !state.sidebarCollapsed }));
  },

  setInspectorPanel: (panel) => {
    set({ inspectorPanel: panel });
  },

  setTheme: (theme) => {
    set({ theme });
  },
}));
```

### Type System Reorganization

#### **Current Issues:**
```typescript
// Current: src/types/domain.ts (417 lines)
// Issues:
// - Mixed domain, API, and UI types
// - Some `any` escapes in components
// - No separation between internal and external types
// - Missing interfaces for platform abstractions
```

#### **New Type System Structure:**

```typescript
// src/types/domain/flow.ts (FLOW DOMAIN TYPES)
export interface FlowDocument {
  id: string;
  name: string;
  description?: string;
  content: FlowContent;
  metadata: FlowMetadata;
  ownership: FlowOwnership;
  permissions: FlowPermission[];
  version: number;
  createdAt: Date;
  updatedAt: Date;
}

export interface FlowOwnership {
  ownerId: string;
  organizationId?: string;
  isPublic: boolean;
}

export interface FlowPermission {
  userId: string;
  permission: 'read' | 'write' | 'admin';
  grantedAt: Date;
}

// src/types/api/requests.ts (API REQUEST TYPES)
export interface LoadFlowRequest {
  flowId: string;
  includeContent?: boolean;
  version?: number;
}

export interface CreateFlowRequest {
  name: string;
  description?: string;
  content: FlowContent;
  organizationId?: string;
  isPublic?: boolean;
}

export interface ShareFlowRequest {
  flowId: string;
  userId: string;
  permission: 'read' | 'write' | 'admin';
}

// src/types/api/responses.ts (API RESPONSE TYPES)
export interface LoadFlowResponse {
  flow: FlowDocument;
  permissions: FlowPermission[];
  canEdit: boolean;
}

export interface CreateFlowResponse {
  flowId: string;
  version: number;
  createdAt: Date;
}

// src/types/ui/components.ts (UI COMPONENT TYPES)
export interface FlowViewProps {
  flow: FlowDocument;
  selectedBlockId?: string;
  onBlockSelect?: (blockId: string) => void;
  readonly?: boolean;
}

export interface BlockViewProps {
  block: Block;
  isSelected?: boolean;
  onSelect?: () => void;
  depth?: number;
}

// src/platform/types.ts (PLATFORM TYPES)
export interface PlatformAdapter {
  getBackendConfig(): Promise<BackendConfig>;
  fileOpen(options: FileOpenOptions): Promise<string | null>;
  fileSave(options: FileSaveOptions): Promise<string | null>;
  fileReveal(path: string): Promise<void>;
}

export interface BackendConfig {
  apiUrl: string;
  token?: string;
  version?: string;
}

export interface FileOpenOptions {
  filters?: FileFilter[];
  multiple?: boolean;
}

export interface FileSaveOptions {
  defaultPath?: string;
  filters?: FileFilter[];
}
```

---

## PART 4: NEW ARCHITECTURE STRUCTURE

### Complete New Backend Structure

```
internal/
├── api/                          # API layer
│   ├── handlers/                # Request handlers by domain
│   │   ├── auth_handler.go       # Authentication endpoints
│   │   ├── flow_handler.go       # Flow CRUD endpoints
│   │   ├── chat_handler.go       # Chat/Streaming endpoints
│   │   ├── org_handler.go        # Organization management
│   │   ├── library_handler.go    # Library management
│   │   └── analysis_handler.go   # Analysis endpoints
│   ├── middleware/               # HTTP middleware
│   │   ├── auth.go              # Authentication middleware
│   │   ├── cors.go              # CORS handling
│   │   ├── rate_limit.go        # Rate limiting
│   │   └── recovery.go          # Panic recovery
│   ├── responses/                # Response utilities
│   │   ├── json.go              # JSON responses
│   │   ├── errors.go            # Error responses
│   │   └── pagination.go        # Pagination helpers
│   ├── router.go                # Route configuration
│   └── server.go                # Server setup
├── auth/                        # Authentication & authorization
│   ├── providers/               # Auth providers
│   │   ├── github.go            # GitHub OAuth
│   │   ├── azure.go             # Azure AD
│   │   ├── google.go            # Google OAuth
│   │   └── local.go             # Local dev auth
│   ├── jwt.go                   # JWT token management
│   ├── permissions.go            # Permission system
│   ├── roles.go                 # Role definitions
│   └── middleware.go            # Auth middleware
├── collaboration/               # Team collaboration
│   ├── org_service.go           # Organization management
│   ├── library_service.go       # Flow libraries
│   ├── permission_service.go    # Permission management
│   ├── activity_service.go      # Activity tracking
│   └── sharing_service.go       # Flow sharing
├── config/                      # Configuration management
│   ├── config.go                # Configuration structure
│   ├── loader.go                # Config loading
│   ├── validator.go             # Config validation
│   ├── local.go                 # Local config
│   └── cloud.go                 # Cloud config
├── database/                    # Database layer
│   ├── postgres/                # PostgreSQL implementation
│   │   ├── db.go               # Database connection
│   │   ├── migrations.go       # Migration management
│   │   └── repositories/       # Repository pattern
│   │       ├── user_repo.go    # User repository
│   │       ├── org_repo.go     # Organization repository
│   │       ├── flow_repo.go    # Flow repository
│   │       └── permission_repo.go # Permission repository
│   ├── sqlite/                  # SQLite implementation (local)
│   │   ├── db.go               # SQLite connection
│   │   └── repositories/       # SQLite repositories
│   └── interfaces/               # Database interfaces
│       └── database.go         # Database interface
├── migration/                   # Data migration
│   ├── migrator.go             # Migration orchestration
│   ├── validator.go            # Data validation
│   ├── batch_processor.go      # Batch processing
│   └── rollback.go             # Rollback handling
├── models/                      # Domain models
│   ├── user.go                  # User model
│   ├── organization.go          # Organization model
│   ├── flow.go                  # Flow model
│   ├── permission.go            # Permission model
│   └── activity.go              # Activity model
├── services/                    # Business logic services
│   ├── flow_service.go          # Flow business logic
│   ├── chat_service.go          # Chat business logic
│   ├── analysis_service.go      # Analysis business logic
│   ├── export_service.go        # Export business logic
│   └── search_service.go        # Search business logic
├── storage/                     # Storage abstraction
│   ├── interfaces/               # Storage interfaces
│   │   └── storage.go          # Storage interface
│   ├── filesystem/              # File system storage
│   │   ├── flow_storage.go     # Flow file operations
│   │   ├── settings_storage.go # Settings file operations
│   │   └── cache_storage.go    # Cache operations
│   ├── database/                # Database storage
│   │   ├── flow_storage.go     # Flow DB operations
│   │   ├── settings_storage.go # Settings DB operations
│   │   └── conversation_storage.go # Chat storage
│   └── cloud/                   # Cloud storage
│       ├── blob_storage.go     # Azure Blob Storage
│       └── key_vault.go        # Azure Key Vault
├── websocket/                   # Real-time collaboration
│   ├── hub.go                   # WebSocket hub
│   ├── client.go                # Client management
│   ├── handlers/                # WebSocket handlers
│   │   ├── flow_handler.go     # Flow collaboration
│   │   └── presence_handler.go # Presence updates
│   └── events.go                # Event types
├── analytics/                   # Analytics & monitoring
│   ├── tracker.go               # Event tracking
│   ├── metrics.go               # Metrics collection
│   └── insights.go              # Azure Application Insights
├── testing/                     # Testing utilities
│   ├── testsuite.go             # Test suite setup
│   ├── mocks/                   # Mock implementations
│   │   ├── api_mock.go         # API mocks
│   │   ├── storage_mock.go     # Storage mocks
│   │   └── auth_mock.go        # Auth mocks
│   └── fixtures/                # Test data
│       ├── flows.go            # Sample flows
│       └── users.go            # Sample users
└── utils/                       # Utility functions
    ├── validation.go            # Input validation
    ├── formatting.go            # Data formatting
    ├── errors.go                # Error handling
    └── logging.go               # Logging utilities
```

### Complete New Frontend Structure

```
frontend/src/
├── platform/                    # Platform abstraction layer
│   ├── adapters/
│   │   ├── TauriAdapter.ts     # Tauri implementations
│   │   ├── WebAdapter.ts       # Web implementations
│   │   └── index.ts            # Adapter factory
│   ├── guards.ts               # Platform detection
│   ├── contexts.tsx            # Platform context
│   └── types.ts                # Platform types
├── services/                   # Business logic layer
│   ├── api/
│   │   ├── ApiClient.ts        # Generic API client
│   │   ├── FlowApi.ts          # Flow API
│   │   ├── ChatApi.ts          # Chat API
│   │   ├── AuthApi.ts          # Auth API
│   │   └── orgApi.ts           # Organization API
│   ├── collaboration/
│   │   ├── CollaborationService.ts  # Real-time collaboration
│   │   ├── PermissionService.ts     # Permissions
│   │   └── PresenceService.ts       # Presence indicators
│   ├── sync/
│   │   ├── SyncManager.ts      # Sync orchestration
│   │   ├── ConflictResolver.ts # Conflict resolution
│   │   └── OfflineManager.ts   # Offline support
│   └── storage/
│       ├── IndexedDBStorage.ts # Browser storage
│       ├── CacheManager.ts     # Response caching
│       └── SecureStorage.ts    # Secure key storage
├── stores/                     # State management
│   ├── domain/                 # Domain state
│   │   ├── flowStore.ts        # Flow state
│   │   ├── chatStore.ts        # Chat state
│   │   ├── authStore.ts        # Auth state
│   │   └── orgStore.ts         # Organization state
│   └── ui/                     # UI state
│       ├── uiStore.ts          # UI preferences
│       └── settingsStore.ts    # Settings
├── components/                 # React components
│   ├── auth/                   # Authentication
│   │   ├── containers/
│   │   │   ├── LoginFormContainer.tsx
│   │   │   └── RegisterFormContainer.tsx
│   │   └── views/
│   │       ├── LoginForm.tsx
│   │       └── RegisterForm.tsx
│   ├── library/                # Library browsing
│   │   ├── containers/
│   │   │   └── LibraryBrowserContainer.tsx
│   │   └── views/
│   │       ├── LibraryBrowser.tsx
│   │       ├── FlowCard.tsx
│   │       └── LibraryNav.tsx
│   ├── sharing/                # Flow sharing
│   │   ├── containers/
│   │   │   └── ShareDialogContainer.tsx
│   │   └── views/
│   │       ├── ShareDialog.tsx
│   │       ├── PermissionSelect.tsx
│   │       └── CollaboratorList.tsx
│   ├── collaboration/          # Real-time collaboration
│   │   ├── containers/
│   │   │   └── PresenceIndicatorsContainer.tsx
│   │   └── views/
│   │       ├── PresenceIndicators.tsx
│   │       ├── CollaborativeCursors.tsx
│   │       └── ConflictResolution.tsx
│   ├── flow/                   # Flow visualization
│   │   ├── containers/
│   │   │   ├── FlowViewContainer.tsx
│   │   │   └── BlockViewContainer.tsx
│   │   └── views/
│   │       ├── FlowView.tsx
│   │       ├── BlockView.tsx
│   │       └── SubflowView.tsx
│   ├── chat/                   # Chat interface
│   │   ├── containers/
│   │   │   ├── AITabContainer.tsx
│   │   │   └── ChatInputContainer.tsx
│   │   └── views/
│   │       ├── AITab.tsx
│   │       ├── ChatInput.tsx
│   │       └── MessageList.tsx
│   ├── layout/                 # Layout components
│   └── shared/                 # Shared components
├── hooks/                      # Custom React hooks
│   ├── usePlatform.ts          # Platform detection
│   ├── useAuth.ts              # Authentication
│   ├── usePermissions.ts       # Permission checking
│   ├── useCollaboration.ts     # Real-time collaboration
│   ├── useSync.ts              # Sync management
│   └── useService.ts           # Service injection
├── types/                      # TypeScript types
│   ├── domain/                 # Domain types
│   │   ├── flow.ts
│   │   ├── chat.ts
│   │   ├── organization.ts
│   │   └── permissions.ts
│   ├── api/                    # API types
│   │   ├── requests.ts
│   │   ├── responses.ts
│   │   └── errors.ts
│   └── ui/                     # UI types
│       ├── components.ts
│       └── layouts.ts
├── utils/                      # Utility functions
│   ├── validation.ts           # Input validation
│   ├── formatting.ts           # Data formatting
│   ├── errorHandling.ts        # Error handling
│   └── dateUtils.ts            # Date utilities
├── testing/                    # Testing utilities
│   ├── testHelpers.tsx         # Test helpers
│   ├── mocks.ts                # Mock implementations
│   └── fixtures.ts             # Test fixtures
└── App.tsx                     # Application root
```

---

## PART 5: IMPLEMENTATION PRIORITIES & TIMELINE

### Phase 1: Foundation & Testing (Weeks 1-8) 
**Priority: CRITICAL - Infrastructure**

#### Backend Foundation (Weeks 1-4)
- [ ] Create storage abstraction layer
- [ ] Implement database interfaces  
- [ ] Set up configuration management
- [ ] Create testing infrastructure
- [ ] Write API handler tests (40+ endpoints)
- [ ] Implement integration test framework

#### Frontend Foundation (Weeks 5-8)
- [ ] Create platform adapter layer
- [ ] Implement service layer extraction
- [ ] Set up component testing infrastructure
- [ ] Write critical component tests
- [ ] Create API client tests
- [ ] Implement platform detection

**Deliverables:**
- Platform-agnostic storage interfaces
- Comprehensive API handler test coverage
- Platform adapter implementation
- Component testing infrastructure
- CI/CD pipeline for tests

### Phase 2: Authentication & Multi-tenancy (Weeks 9-16)
**Priority: HIGH - Core Features**

#### Authentication Implementation (Weeks 9-12)
- [ ] Implement authentication providers (GitHub, Azure, Google)
- [ ] Create JWT token management
- [ ] Build authorization middleware
- [ ] Implement permission system
- [ ] Create authentication UI components
- [ ] Write auth tests

#### Multi-tenancy Implementation (Weeks 13-16)
- [ ] Create organization service
- [ ] Implement flow libraries
- [ ] Build permission system
- [ ] Create team collaboration UI
- [ ] Implement activity logging
- [ ] Write multi-tenancy tests

**Deliverables:**
- Working authentication system
- Organization management
- Flow libraries with permissions
- Team collaboration UI
- Comprehensive auth/permission tests

### Phase 3: Cloud Deployment & Collaboration (Weeks 17-24)
**Priority: HIGH - Production Ready**

#### Cloud Infrastructure (Weeks 17-20)
- [ ] Set up Azure resources (PostgreSQL, Redis, Blob Storage)
- [ ] Implement cloud configuration
- [ ] Create connection pooling
- [ ] Set up CI/CD pipelines
- [ ] Configure monitoring & logging
- [ ] Implement deployment automation

#### Real-time Collaboration (Weeks 21-24)
- [ ] Implement WebSocket support
- [ ] Create presence indicators
- [ ] Build conflict resolution
- [ ] Implement offline support
- [ ] Create sync mechanisms
- [ ] Write collaboration tests

**Deliverables:**
- Production cloud deployment
- Real-time collaboration features
- Offline support
- Monitoring and logging
- Automated deployment pipeline

---

## SUMMARY OF CRITICAL CHANGES

### Files Requiring Complete Rewrite (Backend)
```
internal/api/handlers.go          → internal/api/handlers/*.go (split)
internal/service/flow.go          → Refactor for multi-tenancy
internal/service/chat.go          → Add streaming + auth
internal/storage/settings.go      → Database abstraction
internal/manager/manager.go        → Cloud mode support
```

### Files Requiring Complete Rewrite (Frontend)  
```
src/App.tsx                       → Platform detection + routing
src/api/client.ts                 → Platform adapter pattern
src/components/chat/AITab.tsx     → Container/view split
src/components/sidebar/Sidebar.tsx → Container/view split
src/stores/flowStore.ts           → Domain state only
src/stores/settingsStore.ts       → Cloud sync + validation
```

### Critical New Files (Backend)
```
internal/auth/                     # Authentication service
internal/collaboration/           # Team collaboration
internal/database/                # Database layer
internal/config/cloud.go          # Cloud configuration
internal/websocket/               # Real-time collaboration
internal/migration/               # Data migration
internal/testing/testsuite.go    # Test infrastructure
```

### Critical New Files (Frontend)
```
src/platform/adapters/            # Platform adapters
src/services/                     # Business logic layer
src/stores/authStore.ts          # Authentication state
src/stores/orgStore.ts           # Organization state
src/components/auth/              # Authentication UI
src/components/library/           # Library browsing
src/components/sharing/           # Flow sharing
src/components/collaboration/    # Real-time collaboration
```

### Test Files to Create (Backend)
```
internal/api/handlers_test.go    # 40+ endpoint tests
internal/auth/middleware_test.go # Auth tests
internal/collaboration/*_test.go # Collaboration tests
internal/testing/integration/    # Integration tests
```

### Test Files to Create (Frontend)
```
src/components/auth/*.test.tsx   # Auth component tests
src/components/flow/*.test.tsx   # Flow component tests
src/services/*.test.ts          # Service layer tests
src/stores/*.test.ts            # State management tests
```

This deep dive provides a comprehensive roadmap for transforming the current single-user desktop application into a modular, multi-platform architecture supporting team collaboration and cloud deployment.