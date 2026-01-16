<script setup lang="ts">
import {computed} from 'vue';
import {SvgIcon} from '../svg.ts';
import type {ActionsJob, ActionsRunStatus} from '../modules/gitea-actions.ts';
import ActionRunStatus from './ActionRunStatus.vue';

const emit = defineEmits<{
  (e: 'toggle', jobID: number): void;
}>();

const props = defineProps<{
  job: ActionsJob;
  depth: number;
  selectedJobId: number;
  runLink: string;
  locale: Record<string, any>;
  isExpanded: (jobID: number) => boolean;
}>();

const href = computed(() => props.job.link || `${props.runLink}/jobs/${props.job.id}`);
const rerunURL = computed(() => `${href.value}/rerun`);
const localeStatus = computed<Record<ActionsRunStatus, string>>(() => props.locale.status);
const hasChildren = computed(() => (props.job.children?.length || 0) > 0);
const expanded = computed(() => props.isExpanded(props.job.id));
</script>

<template>
  <a class="job-brief-item" :href="href" :class="selectedJobId === job.id ? 'selected' : ''">
    <div class="job-brief-item-left">
      <span class="job-brief-indent" :style="{width: `${depth * 16}px`}"/>
      <button
        v-if="hasChildren"
        class="job-brief-toggle"
        type="button"
        :aria-label="expanded ? 'Collapse' : 'Expand'"
        @click.stop.prevent="emit('toggle', job.id)"
      >
        <SvgIcon name="octicon-chevron-right" class="job-brief-toggle-icon" :class="{expanded}"/>
      </button>
      <span v-else class="job-brief-toggle-spacer"/>
      <ActionRunStatus :locale-status="localeStatus[job.status]" :status="job.status"/>
      <span class="job-brief-name tw-mx-2 gt-ellipsis">{{ job.name }}</span>
    </div>
    <span class="job-brief-item-right">
      <SvgIcon
        name="octicon-sync"
        role="button"
        :data-tooltip-content="locale.rerun"
        class="job-brief-rerun tw-mx-2 link-action interact-fg"
        :data-url="rerunURL"
        v-if="job.canRerun"
      />
      <span class="step-summary-duration">{{ job.duration }}</span>
    </span>
  </a>

  <ActionJobTreeItem
    v-for="child in expanded ? (job.children || []) : []"
    :key="child.id"
    :job="child"
    :depth="depth + 1"
    :selected-job-id="selectedJobId"
    :run-link="runLink"
    :locale="locale"
    :is-expanded="isExpanded"
    @toggle="emit('toggle', $event)"
  />
</template>

<style scoped>
.job-brief-item {
  padding: 10px;
  border-radius: var(--border-radius);
  text-decoration: none;
  display: flex;
  flex-wrap: nowrap;
  justify-content: space-between;
  align-items: center;
  color: var(--color-text);
}

.job-brief-item:hover {
  background-color: var(--color-hover);
}

.job-brief-item.selected {
  font-weight: var(--font-weight-bold);
  background-color: var(--color-active);
}

.job-brief-item .job-brief-rerun {
  cursor: pointer;
}

.job-brief-item .job-brief-item-left {
  display: flex;
  width: 100%;
  min-width: 0;
  align-items: center;
}

.job-brief-toggle,
.job-brief-toggle-spacer {
  width: 16px;
  height: 16px;
  flex: 0 0 auto;
  margin-left: 2px;
}

.job-brief-toggle {
  border: 0;
  padding: 0;
  background: transparent;
  cursor: pointer;
  color: var(--color-text);
  display: inline-flex;
  align-items: center;
  justify-content: center;
}

.job-brief-toggle:hover {
  color: var(--color-primary);
}

.job-brief-toggle-icon {
  transform: rotate(0deg);
  transition: transform 0.15s ease;
}

.job-brief-toggle-icon.expanded {
  transform: rotate(90deg);
}

.job-brief-item .job-brief-name {
  display: block;
  width: 70%;
}

.job-brief-item .job-brief-item-right {
  display: flex;
  align-items: center;
}

.job-brief-indent {
  flex: 0 0 auto;
}
</style>
