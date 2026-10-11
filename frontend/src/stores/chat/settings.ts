import type {StateCreator} from 'zustand'
import type {ChatState} from './state'

type SettingsSlice = Pick<
  ChatState,
  'setProvider' | 'bumpProviderEpoch' | 'openModelPicker' | 'setModelRefusal' | 'setStagedPrompt' | 'setDraft'
>

export const createSettingsSlice: StateCreator<ChatState, [], [], SettingsSlice> = set => ({
  setProvider: p => set({selectedProvider: p}),

  bumpProviderEpoch: () => set(s => ({providerEpoch: s.providerEpoch + 1})),
  openModelPicker: () => set(s => ({modelPickerRequest: s.modelPickerRequest + 1})),
  setModelRefusal: (threadId, refusal) =>
    set(state => {
      if (refusal) return {modelRefusals: {...state.modelRefusals, [threadId]: refusal}}
      if (!(threadId in state.modelRefusals)) return state
      const modelRefusals = {...state.modelRefusals}
      delete modelRefusals[threadId]
      return {modelRefusals}
    }),
  setStagedPrompt: p => set({stagedPrompt: p}),

  setDraft: (threadId, text) =>
    set(state => {
      if (!text) {
        if (!(threadId in state.drafts)) return state
        const drafts = {...state.drafts}
        delete drafts[threadId]
        return {drafts}
      }
      if (state.drafts[threadId] === text) return state
      return {drafts: {...state.drafts, [threadId]: text}}
    }),
})
