<script setup lang="ts">
import {ref, watch} from 'vue';
import type {ActionsJob} from '../modules/gitea-actions.ts';
import ActionJobTreeItem from './ActionJobTreeItem.vue';

const props = defineProps<{
  jobs: ActionsJob[];
  selectedJobId: number;
  runLink: string;
  locale: Record<string, any>;
}>();

const expanded = ref<Set<number>>(new Set());

function toggleExpanded(jobID: number) {
  if (expanded.value.has(jobID)) {
    expanded.value.delete(jobID);
  } else {
    expanded.value.add(jobID);
  }
}

function isExpanded(jobID: number) {
  return expanded.value.has(jobID);
}

function findAncestorIDs(jobs: ActionsJob[], targetID: number) {
  const path: number[] = [];

  const dfs = (nodes: ActionsJob[]): boolean => {
    for (const node of nodes) {
      if (node.id === targetID) return true;
      if (node.children?.length) {
        path.push(node.id);
        if (dfs(node.children)) return true;
        path.pop();
      }
    }
    return false;
  };

  dfs(jobs);
  return path;
}

watch(
  () => [props.jobs, props.selectedJobId] as const,
  () => {
    if (!props.selectedJobId) return;
    for (const id of findAncestorIDs(props.jobs, props.selectedJobId)) {
      expanded.value.add(id);
    }
  },
  {immediate: true},
);
</script>

<template>
  <div class="job-brief-list">
    <ActionJobTreeItem
      v-for="job in jobs"
      :key="job.id"
      :job="job"
      :depth="0"
      :selected-job-id="selectedJobId"
      :run-link="runLink"
      :locale="locale"
      :is-expanded="isExpanded"
      @toggle="toggleExpanded"
    />
  </div>
</template>

<style scoped>
.job-brief-list {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
</style>
