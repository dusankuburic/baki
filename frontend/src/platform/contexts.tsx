/**
 * Platform context provider for React components
 */

import { createContext, useContext, useEffect, useState, type ReactNode } from 'react';
import type { PlatformAdapter, PlatformInfo, BackendConfig } from './types';
import { createAdapter, getPlatformType, getPlatformCapabilities } from './adapters';

/**
 * Platform context interface
 */
interface PlatformContextType {
  adapter: PlatformAdapter;
  platformInfo: PlatformInfo;
  backendConfig: BackendConfig | null;
  isReady: boolean;
  error: Error | null;
}

/**
 * Platform context
 */
const PlatformContext = createContext<PlatformContextType | null>(null);

/**
 * Platform provider props
 */
interface PlatformProviderProps {
  children: ReactNode;
}

/**
 * Platform provider component
 */
export function PlatformProvider({ children }: PlatformProviderProps) {
  const [adapter] = useState<PlatformAdapter>(() => createAdapter());
  const [backendConfig, setBackendConfig] = useState<BackendConfig | null>(null);
  const [isReady, setIsReady] = useState(false);
  const [error, setError] = useState<Error | null>(null);

  const rawType = getPlatformType();
  const platformInfo: PlatformInfo = {
    type: rawType === 'unknown' ? 'web' : rawType,
    capabilities: getPlatformCapabilities(),
  };

  useEffect(() => {
    const initializePlatform = async () => {
      try {
        const config = await adapter.getBackendConfig();
        setBackendConfig(config);
        setIsReady(true);
      } catch (err) {
        console.error('Failed to initialize platform:', err);
        setError(err as Error);
        setIsReady(true);
      }
    };

    initializePlatform();
  }, [adapter]);

  const contextValue: PlatformContextType = {
    adapter,
    platformInfo,
    backendConfig,
    isReady,
    error,
  };

  return (
    <PlatformContext.Provider value={contextValue}>
      {children}
    </PlatformContext.Provider>
  );
}

/**
 * Hook to use platform context
 */
export function usePlatformContext(): PlatformContextType {
  const context = useContext(PlatformContext);
  if (!context) {
    throw new Error('usePlatformContext must be used within PlatformProvider');
  }
  return context;
}

/**
 * Hook to get platform adapter
 */
export function usePlatformAdapter(): PlatformAdapter {
  const { adapter } = usePlatformContext();
  return adapter;
}

/**
 * Hook to get backend config
 */
export function useBackendConfig(): BackendConfig | null {
  const { backendConfig } = usePlatformContext();
  return backendConfig;
}