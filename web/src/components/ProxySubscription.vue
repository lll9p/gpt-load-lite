<script setup lang="ts">
import {
  subscriptionApi,
  subscriptionErrorDetail,
  type SubscriptionDiff,
  type SubscriptionInput,
  type SubscriptionStatus,
} from "@/api/outbound-proxies";
import {
  NAlert,
  NButton,
  NCheckbox,
  NFormItem,
  NInput,
  NInputNumber,
  NModal,
  NSelect,
  NSpace,
  NText,
} from "naive-ui";
import { computed, onMounted, onUnmounted, reactive, ref, watch } from "vue";
import { useI18n } from "vue-i18n";

const props = defineProps<{ disabled: boolean }>();
const emit = defineEmits<{ status: [SubscriptionStatus | undefined]; changed: [] }>();
const { t, locale } = useI18n();
const status = ref<SubscriptionStatus>();
const known = ref(false);
const show = ref(false);
const ready = ref(false);
const busy = ref(false);
const error = ref<string>();
const confirmed = ref(false);
const preview = ref<SubscriptionDiff & { preview_token: string }>();
const form = reactive<
  Omit<SubscriptionInput, "interval_minutes" | "preview_token"> & {
    interval_minutes: number | null;
  }
>({
  mode: "manual",
  format: "webshare",
  protocol: "socks5h",
  auto_refresh: false,
  interval_minutes: 60,
  url: "",
  clear_url: false,
});
const requiresPreview = computed(
  () =>
    form.mode === "subscription" &&
    (status.value?.mode !== "subscription" ||
      Boolean(form.url?.trim()) ||
      form.format !== status.value.format ||
      form.protocol !== status.value.protocol)
);

function clearPreview() {
  preview.value = undefined;
  confirmed.value = false;
}
watch(form, clearPreview, { flush: "sync" });
watch(
  () => form.mode,
  () => {
    form.clear_url = false;
  }
);

// Ignore a background response that started before a modal load or mutation.
let revision = 0;
let polling = false;
async function syncStatus(version: number, reload = false) {
  try {
    const value = await subscriptionApi.get();
    if (version === revision) {
      accept(value, reload);
    }
  } catch {
    if (version === revision) {
      known.value = false;
      emit("status", undefined);
      if (reload) {
        emit("changed");
      }
    }
  }
}

function accept(value: SubscriptionStatus, reload = false) {
  const previous = status.value;
  const poolChanged =
    previous &&
    (previous.last_success_at !== value.last_success_at || previous.mode !== value.mode);
  if (
    poolChanged ||
    previous?.format !== value.format ||
    previous?.protocol !== value.protocol ||
    previous?.has_url !== value.has_url
  ) {
    clearPreview();
  }
  status.value = value;
  known.value = true;
  if (show.value && !ready.value) {
    Object.assign(form, {
      mode: value.mode,
      format: value.format,
      protocol: value.protocol,
      auto_refresh: value.auto_refresh,
      interval_minutes: value.interval_minutes,
      url: "",
      clear_url: false,
    });
    ready.value = true;
  }
  emit("status", value);
  if (reload || poolChanged) {
    emit("changed");
  }
}

async function poll() {
  if (busy.value || polling) {
    return;
  }
  polling = true;
  try {
    await syncStatus(revision);
  } finally {
    polling = false;
  }
}

async function reloadStatus() {
  busy.value = true;
  try {
    await syncStatus(++revision);
  } finally {
    busy.value = false;
  }
}

function open() {
  if (props.disabled || show.value) {
    return;
  }
  ready.value = false;
  error.value = undefined;
  show.value = true;
  void reloadStatus();
}

function close() {
  if (busy.value) {
    return;
  }
  show.value = false;
  form.url = "";
  form.clear_url = false;
  clearPreview();
}

const input = computed<SubscriptionInput | undefined>(() => {
  const saved = status.value;
  if (form.mode === "manual" && saved) {
    return {
      mode: "manual",
      format: saved.format,
      protocol: saved.protocol,
      auto_refresh: saved.auto_refresh,
      interval_minutes: saved.interval_minutes,
      clear_url: (saved.has_url && form.clear_url) || undefined,
    };
  }
  const interval = form.interval_minutes;
  if (interval === null || !Number.isInteger(interval) || interval < 5 || interval > 1440) {
    return;
  }
  const { mode, format, protocol, auto_refresh, url } = form;
  const link = url?.trim();
  if (!link && !status.value?.has_url) {
    return;
  }
  return {
    mode,
    format,
    protocol,
    auto_refresh,
    interval_minutes: interval,
    url: link || undefined,
  };
});
const canSave = computed(
  () => input.value && (!requiresPreview.value || (preview.value && confirmed.value))
);

async function run(action: "preview" | "save" | "refresh") {
  if (busy.value || !known.value || (action === "save" && !canSave.value)) {
    return;
  }
  const draft = input.value;
  if (action !== "refresh" && !draft) {
    return;
  }
  busy.value = true;
  const version = ++revision;
  error.value = undefined;
  try {
    if (action === "preview" && draft) {
      clearPreview();
      const result = await subscriptionApi.preview(draft);
      if (version === revision) {
        preview.value = result;
      }
    } else if (action === "save" && draft) {
      const saved = await subscriptionApi.save({
        ...draft,
        preview_token: requiresPreview.value ? preview.value?.preview_token : undefined,
      });
      if (version === revision) {
        accept(saved, true);
        busy.value = false;
        close();
      }
    } else if (action === "refresh") {
      await subscriptionApi.refresh();
      if (version === revision) {
        clearPreview();
        await syncStatus(version, true);
      }
    }
  } catch (cause) {
    if (version !== revision) {
      return;
    }
    error.value =
      subscriptionErrorDetail(cause) ??
      t(action === "save" ? "proxySubscription.saveFailed" : "proxySubscription.operationFailed");
    if (action !== "preview") {
      // A failed response can follow a committed mutation. Never reuse its preview token.
      clearPreview();
      await syncStatus(version, true);
    }
  } finally {
    busy.value = false;
  }
}

function dateLabel(value?: string, compact = false) {
  if (!value) {
    return "—";
  }
  const date = new Date(value);
  return compact
    ? date.toLocaleTimeString(locale.value, { hour: "2-digit", minute: "2-digit" })
    : date.toLocaleString(locale.value);
}

let timer: ReturnType<typeof setInterval>;
onMounted(() => {
  void poll();
  timer = setInterval(poll, 15000);
});
onUnmounted(() => {
  clearInterval(timer);
  revision++;
});
</script>

<template>
  <n-button :disabled="props.disabled" @click="open">{{ t("proxySubscription.title") }}</n-button>
  <n-text v-if="known && status?.mode === 'subscription'" depth="3">
    {{ t(status.running ? "proxySubscription.running" : "proxySubscription.active") }}
    · {{ t(`proxySubscription.${status.format}`) }} ·
    {{
      t(status.auto_refresh ? "proxySubscription.every" : "proxySubscription.autoOff", {
        count: status.interval_minutes,
      })
    }}
    · {{ t("proxySubscription.success") }} {{ dateLabel(status.last_success_at, true) }} ·
    {{ t("proxySubscription.next") }} {{ dateLabel(status.next_refresh_at, true) }}
  </n-text>
  <n-modal
    :show="show"
    preset="card"
    style="width: 620px; max-width: 95vw"
    :title="t('proxySubscription.title')"
    :mask-closable="false"
    :closable="!busy"
    :close-on-esc="!busy"
    @update:show="!$event && close()"
  >
    <n-space vertical>
      <n-text v-if="busy && !ready">{{ t("outboundProxies.loading") }}</n-text>
      <n-alert v-else-if="!known" type="warning">
        {{ t("proxySubscription.unknown") }}
        <n-button size="small" :disabled="busy" @click="reloadStatus">
          {{ t("outboundProxies.refresh") }}
        </n-button>
      </n-alert>
      <n-alert v-if="error" type="error">{{ error }}</n-alert>
      <template v-if="ready">
        <n-form-item :label="t('proxySubscription.mode')">
          <n-select
            v-model:value="form.mode"
            :disabled="busy || !known"
            :options="
              ['manual', 'subscription'].map(value => ({
                value,
                label: t(`proxySubscription.${value}`),
              }))
            "
          />
        </n-form-item>
        <template v-if="form.mode === 'subscription'">
          <n-alert v-if="requiresPreview" type="warning">
            {{ t("proxySubscription.takeover") }}
          </n-alert>
          <n-form-item :label="t('proxySubscription.format')">
            <n-select
              v-model:value="form.format"
              :disabled="busy || !known"
              :options="
                ['webshare', 'url_list'].map(value => ({
                  value,
                  label: t(`proxySubscription.${value}`),
                }))
              "
            />
          </n-form-item>
          <n-form-item v-if="form.format === 'webshare'" :label="t('proxySubscription.protocol')">
            <n-select
              v-model:value="form.protocol"
              :disabled="busy || !known"
              :options="
                ['socks5h', 'http', 'https', 'socks5'].map(value => ({ value, label: value }))
              "
            />
          </n-form-item>
          <n-form-item :label="t('proxySubscription.url')">
            <n-input
              v-model:value="form.url"
              type="password"
              autocomplete="new-password"
              :disabled="busy || !known"
              :placeholder="
                t(status?.has_url ? 'proxySubscription.keepUrl' : 'proxySubscription.enterUrl')
              "
            />
          </n-form-item>
          <n-checkbox v-model:checked="form.auto_refresh" :disabled="busy || !known">
            {{ t("proxySubscription.auto") }}
          </n-checkbox>
          <n-form-item :label="t('proxySubscription.interval')">
            <n-input-number
              v-model:value="form.interval_minutes"
              :min="5"
              :max="1440"
              :precision="0"
              :disabled="busy || !known"
            />
          </n-form-item>
          <n-button :disabled="busy || !known || !input" @click="run('preview')">
            {{ t("proxySubscription.preview") }}
          </n-button>
          <n-alert v-if="preview" type="info">
            {{ t("proxySubscription.diff", { ...preview }) }}
          </n-alert>
          <n-checkbox
            v-if="requiresPreview && preview"
            v-model:checked="confirmed"
            :disabled="busy || !known"
          >
            {{ t("proxySubscription.confirm") }}
          </n-checkbox>
        </template>
        <template v-else>
          <n-alert v-if="status?.mode === 'subscription'" type="info">
            {{ t("proxySubscription.keepPool") }}
          </n-alert>
          <n-checkbox
            v-if="status?.has_url"
            v-model:checked="form.clear_url"
            :disabled="busy || !known"
          >
            {{ t("proxySubscription.clearUrl") }}
          </n-checkbox>
        </template>
      </template>
      <n-space>
        <n-button
          type="primary"
          :disabled="busy || !ready || !known || !canSave"
          @click="run('save')"
        >
          {{ t("common.save") }}
        </n-button>
        <n-button
          v-if="status?.mode === 'subscription'"
          :disabled="busy || !known || status.running"
          @click="run('refresh')"
        >
          {{ t("proxySubscription.refresh") }}
        </n-button>
        <n-button :disabled="busy" @click="close">{{ t("common.cancel") }}</n-button>
      </n-space>
      <template v-if="status && known && ready">
        <n-text v-if="status.running">{{ t("proxySubscription.running") }}</n-text>
        <n-text>
          {{ t("proxySubscription.attempt") }}: {{ dateLabel(status.last_attempt_at) }}
        </n-text>
        <n-text>
          {{ t("proxySubscription.success") }}: {{ dateLabel(status.last_success_at) }}
        </n-text>
        <n-text>{{ t("proxySubscription.next") }}: {{ dateLabel(status.next_refresh_at) }}</n-text>
        <n-alert v-if="status.last_error" type="error">{{ status.last_error }}</n-alert>
        <n-text v-if="status.last_result">
          {{ t("proxySubscription.diff", { ...status.last_result }) }}
        </n-text>
      </template>
    </n-space>
  </n-modal>
</template>
