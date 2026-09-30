const KLING_CLASSIC_MODELS = [
  "kling-v1",
  "kling-v1-5",
  "kling-v1-6",
  "kling-v2-master",
  "kling-v2-1",
  "kling-v2-1-master",
  "kling-v2-5-turbo",
  "kling-v2-6",
  "kling-v3",
  "kling-video-o1",
  "kling-v3-omni",
];

const KLING_NEW_MODELS = [
  "kling-3.0",
  "kling-3.0-turbo",
  "kling-3.0-omni",
  "kling-o1",
  "kling-2.6",
  "kling-2.5-turbo",
];

const KLING_MODELS = KLING_CLASSIC_MODELS.concat(KLING_NEW_MODELS);

// 这些路径均来自可灵原版接口。任务型接口共用宿主的提交、轮询和
// 用户隔离列表；会话类接口保留为同步提交，响应仍由上游决定。
const KLING_ENDPOINTS = {
  text_to_video: { path: "/v1/videos/text2video", task: true },
  image_to_video: { path: "/v1/videos/image2video", task: true },
  omni_video: { path: "/v1/videos/omni-video", task: true },
  multi_image_to_video: { path: "/v1/videos/multi-image2video", task: true },
  motion_control: { path: "/v1/videos/motion-control", task: true },
  multi_elements: { path: "/v1/videos/multi-elements", task: true },
  multi_elements_init: { path: "/v1/videos/multi-elements/init-selection", task: false },
  multi_elements_add: { path: "/v1/videos/multi-elements/add-selection", task: false },
  multi_elements_delete: { path: "/v1/videos/multi-elements/delete-selection", task: false },
  multi_elements_clear: { path: "/v1/videos/multi-elements/clear-selection", task: false },
  multi_elements_preview: { path: "/v1/videos/multi-elements/preview-selection", task: false },
  video_extend: { path: "/v1/videos/video-extend", task: true },
  identify_face: { path: "/v1/videos/identify-face", task: false },
  advanced_lip_sync: { path: "/v1/videos/advanced-lip-sync", task: true },
  avatar_image_to_video: { path: "/v1/videos/avatar/image2video", task: true },
  text_to_audio: { path: "/v1/audio/text-to-audio", task: true },
  video_to_audio: { path: "/v1/audio/video-to-audio", task: true },
  text_to_speech: { path: "/v1/audio/tts", task: true },
  custom_voices: { path: "/v1/general/custom-voices", task: true },
  delete_voices: { path: "/v1/general/delete-voices", task: false },
  presets_voices: { path: "/v1/general/presets-voices", task: false, proxy: true },
  image_recognize: { path: "/v1/videos/image-recognize", task: false },
  advanced_custom_elements: { path: "/v1/general/advanced-custom-elements", task: true },
  delete_elements: { path: "/v1/general/delete-elements", task: false },
  presets_elements: { path: "/v1/general/advanced-presets-elements", task: false, proxy: true },
  effects: { path: "/v1/videos/effects", task: true },
};

const KLING_NEW_ENDPOINTS = {
  new_text_to_video: { path: "/text-to-video", task: true },
  new_image_to_video: { path: "/image-to-video", task: true },
  new_omni_video: { path: "/omni-video", task: true },
  new_motion_control: { path: "/motion-control", task: true },
};

const KLING_NEW_ACTIONS = Object.keys(KLING_NEW_ENDPOINTS);

const KLING_SECONDS_FIELD = {
  type: "number",
  unit: "second",
  displayOrder: 10,
  description: { en: "Video generation unit price", zh: "视频生成单价" },
};

const KLING_RESOLUTION_LABELS = {
  "720p": { en: "720p", zh: "720p" },
  "1080p": { en: "1080p", zh: "1080p" },
  "4k": { en: "4K", zh: "4K" },
};

// v3 的 4K 与普通/动作控制使用不同的价格档，单独作为计费操作以避免
// 在价格矩阵中生成官方不支持的动作控制 4K 组合。
const KLING_V3_USAGE_SCHEMA = {
  operation: {
    enum: ["generation", "motion_control", "4k"],
    enumLabels: {
      generation: { en: "Standard generation", zh: "普通生成" },
      motion_control: { en: "Motion control", zh: "动作控制" },
      "4k": { en: "4K", zh: "4K" },
    },
    description: { en: "Video operation", zh: "视频操作" },
  },
  resolution: {
    enum: ["720p", "1080p", "4k"],
    enumLabels: KLING_RESOLUTION_LABELS,
    when: [
      { field: "operation", values: ["generation", "motion_control"], enum: ["720p", "1080p"] },
      { field: "operation", values: ["4k"], enum: ["4k"] },
    ],
    description: { en: "Output video resolution", zh: "输出视频分辨率" },
  },
  audio: {
    enum: ["silent", "sound"],
    enumLabels: {
      silent: { en: "Silent", zh: "无声" },
      sound: { en: "With sound", zh: "有声" },
    },
    when: [{ field: "operation", values: ["generation", "motion_control"] }],
    description: { en: "Audio output mode", zh: "音频输出模式" },
  },
  seconds: KLING_SECONDS_FIELD,
};

const KLING_OMNI_USAGE_SCHEMA = {
  resolution: {
    enum: ["720p", "1080p", "4k"],
    enumLabels: KLING_RESOLUTION_LABELS,
    description: { en: "Output video resolution", zh: "输出视频分辨率" },
  },
  input_mode: {
    enum: ["silent", "sound", "reference_video"],
    enumLabels: {
      silent: { en: "No reference video · Silent", zh: "无参考视频 · 无声" },
      sound: { en: "No reference video · With sound", zh: "无参考视频 · 有声" },
      reference_video: { en: "With reference video · Silent", zh: "有参考视频 · 无声" },
    },
    when: [{ field: "resolution", values: ["720p", "1080p"] }],
    description: { en: "Input and audio mode", zh: "输入与音频模式" },
  },
  seconds: KLING_SECONDS_FIELD,
};

function klingUsageExamples(examples) {
  return examples.map(function (facts) {
    return { label: facts.label, facts: Object.assign({}, facts.facts) };
  });
}

const KLING_USAGE_PROFILES = [
  {
    models: ["kling-v3"],
    schema: KLING_V3_USAGE_SCHEMA,
    examples: klingUsageExamples([
      { label: "720p · 无声", facts: { seconds: 5, resolution: "720p", operation: "generation", audio: "silent" } },
      { label: "720p · 有声", facts: { seconds: 5, resolution: "720p", operation: "generation", audio: "sound" } },
      { label: "1080p · 无声", facts: { seconds: 5, resolution: "1080p", operation: "generation", audio: "silent" } },
      { label: "1080p · 有声", facts: { seconds: 5, resolution: "1080p", operation: "generation", audio: "sound" } },
      { label: "4k", facts: { seconds: 5, resolution: "4k", operation: "4k", audio: "silent" } },
      { label: "720p · 无声 · 动作控制", facts: { seconds: 5, resolution: "720p", operation: "motion_control", audio: "silent" } },
      { label: "720p · 有声 · 动作控制", facts: { seconds: 5, resolution: "720p", operation: "motion_control", audio: "sound" } },
      { label: "1080p · 无声 · 动作控制", facts: { seconds: 5, resolution: "1080p", operation: "motion_control", audio: "silent" } },
      { label: "1080p · 有声 · 动作控制", facts: { seconds: 5, resolution: "1080p", operation: "motion_control", audio: "sound" } },
    ]),
  },
  {
    models: ["kling-v3-omni"],
    schema: KLING_OMNI_USAGE_SCHEMA,
    examples: klingUsageExamples([
      { label: "720p · 无参考视频 · 无声", facts: { seconds: 5, resolution: "720p", input_mode: "silent" } },
      { label: "720p · 无参考视频 · 有声", facts: { seconds: 5, resolution: "720p", input_mode: "sound" } },
      { label: "720p · 有参考视频 · 无声", facts: { seconds: 5, resolution: "720p", input_mode: "reference_video" } },
      { label: "1080p · 无参考视频 · 无声", facts: { seconds: 5, resolution: "1080p", input_mode: "silent" } },
      { label: "1080p · 无参考视频 · 有声", facts: { seconds: 5, resolution: "1080p", input_mode: "sound" } },
      { label: "1080p · 有参考视频 · 无声", facts: { seconds: 5, resolution: "1080p", input_mode: "reference_video" } },
      { label: "4k", facts: { seconds: 5, resolution: "4k", input_mode: "silent" } },
    ]),
  },
];

function klingRoutes() {
  const routes = [];
  for (const action of Object.keys(KLING_ENDPOINTS)) {
    const endpoint = KLING_ENDPOINTS[action];
    if (endpoint.proxy) {
      routes.push({ method: "GET", path: "/kling" + endpoint.path, type: "dynamic", action: "proxy", decode: "decodeProxy", render: "proxyResponse" });
      continue;
    }
    routes.push({ method: "POST", path: "/kling" + endpoint.path, type: "submit", action: action, decode: "decodeSubmit", render: "taskCreated" });
    if (!endpoint.task) continue;
    routes.push({ method: "GET", path: "/kling" + endpoint.path + "/:task_id", type: "query", render: "taskStatus" });
    routes.push({ method: "GET", path: "/kling" + endpoint.path, type: "dynamic", action: "list", decode: "decodeTaskList", render: "taskList" });
  }
  for (const action of KLING_NEW_ACTIONS) {
    const endpoint = KLING_NEW_ENDPOINTS[action];
    routes.push({ method: "POST", path: "/kling" + endpoint.path + "/:model", type: "submit", action: action, decode: "decodeNewSubmit", render: "newTaskCreated" });
  }
  routes.push({ method: "GET", path: "/kling/tasks", type: "dynamic", action: "list", decode: "decodeNewTaskList", render: "newTaskList" });
  return routes;
}

export const meta = {
  apiVersion: 1,
  key: "kling",
  name: "Kling",
  icon: "Kling.Color",
  description: {
    en: "Kuaishou Kling video generation and media APIs",
    zh: "快手可灵视频生成及媒体接口",
  },
  version: "1.3.1",
  author: { name: "QuantumNous" },
  channelTypes: [50],
  models: KLING_MODELS,
  fetchMode: "per_task",
  upstreams: ["vendor", "new_api"],
  usageSchema: {
    // Kling final unit deduction (estimated at submit, actual on completion).
    units: {
      type: "number",
      unit: "credit",
      description: { en: "Kling credit unit price", zh: "可灵资源包单位单价" },
    },
  },
  usageExamples: [
    { label: "v1 std 5s", facts: { units: 1 } },
    { label: "v1 pro 5s", facts: { units: 3.5 } },
    { label: "v1-6 std 5s", facts: { units: 2 } },
    { label: "v1-6 pro 10s", facts: { units: 7 } },
    { label: "v2-master pro 5s", facts: { units: 10 } },
  ],
  usageProfiles: KLING_USAGE_PROFILES,
  protocols: [{ name: "openai_responses", supports: ["stream", "sync", "background"] }, "openai_video"],
  routes: klingRoutes(),
};

// Official unit consumption (units per output video second), not a currency price.
// Source: https://kling.ai/dev/pricing
const UNITS_PER_SECOND = {
  "kling-v1": { std: 0.2, pro: 0.7 },
  "kling-v1-6": { std: 0.4, pro: 0.7 },
  "kling-v2-master": { pro: 2.0 },
};

const KLING_SYNC_ACTIONS = [
  "multi_elements_init",
  "multi_elements_add",
  "multi_elements_delete",
  "multi_elements_clear",
  "multi_elements_preview",
  "identify_face",
  "text_to_speech",
  "delete_voices",
  "presets_voices",
  "image_recognize",
  "delete_elements",
  "presets_elements",
];

const KLING_MODEL_ACTIONS = [
  "text_to_video",
  "image_to_video",
  "omni_video",
  "multi_image_to_video",
  "motion_control",
  "multi_elements",
];

const KLING_VIDEO_METERED_ACTIONS = KLING_MODEL_ACTIONS.concat(["video_extend", "advanced_lip_sync", "avatar_image_to_video", "effects"], KLING_NEW_ACTIONS);

const MAX_UPSTREAM_UNITS = 1000000;
const MAX_NEW_OUTPUT_SECONDS = 30;

function trimmed(value) {
  return String(value || "").trim();
}

function responsesInput(req) {
  const texts = [],
    images = [];
  const input = req.input;
  if (typeof input === "string") texts.push(input);
  else if (Array.isArray(input)) {
    for (const item of input) {
      if (typeof item === "string") {
        texts.push(item);
        continue;
      }
      if (!item || typeof item !== "object" || Array.isArray(item)) continue;
      const content = item.content === undefined ? [item] : Array.isArray(item.content) ? item.content : [item.content];
      for (const part of content) {
        if (typeof part === "string") {
          texts.push(part);
          continue;
        }
        if (!part || typeof part !== "object" || Array.isArray(part)) continue;
        if (["input_text", "text"].includes(part.type) && typeof part.text === "string") texts.push(part.text);
        if (["input_image", "image_url"].includes(part.type)) {
          let image = part.image_url;
          if (image && typeof image === "object") image = image.url;
          if (trimmed(image)) images.push(trimmed(image));
        }
      }
    }
  }
  return {
    prompt: texts
      .filter(function (text) {
        return trimmed(text);
      })
      .join("\n"),
    images: images,
  };
}

function responsesVideoText(ctx) {
  const artifact = ctx && ctx.artifacts && ctx.artifacts.video;
  const url = trimmed(artifact && artifact.url);
  if (!url) throw new Error("video artifact is unavailable");
  const escaped = url.replace(/&/g, "&amp;").replace(/"/g, "&quot;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  return '<video controls src="' + escaped + '"></video>';
}

function isRelay(apiKey) {
  return apiKey.startsWith("sk-");
}

// The host signal is authoritative on New API channels; the sk- key prefix
// stays as the heuristic for legacy type-50 channels pointed at a gateway.
function viaGateway(ctx) {
  return !!(ctx.upstream && ctx.upstream.kind === "new_api") || isRelay(ctx.apiKey);
}

function tokenFor(ctx) {
  if (viaGateway(ctx)) return ctx.apiKey;
  const parts = ctx.apiKey.split("|");
  if (parts.length !== 2) throw new Error("invalid api_key, required format is accessKey|secretKey");
  const now = utils.unixNow();
  return utils.jwtSignHS256({ iss: parts[0].trim(), exp: now + 1800, nbf: now - 5 }, parts[1].trim());
}

function pathFor(action) {
  const endpoint = KLING_ENDPOINTS[action] || KLING_ENDPOINTS.text_to_video;
  return endpoint.path;
}

function urlFor(ctx, action) {
  return apiRoot(ctx) + pathFor(action);
}

function isNewKlingAction(action) {
  return KLING_NEW_ACTIONS.includes(action);
}

function apiRoot(ctx) {
  const base = trimmed(ctx.baseUrl).replace(/\/+$/, "");
  if (base.toLowerCase().endsWith("/kling")) return base;
  return base + (viaGateway(ctx) ? "/kling" : "");
}

function newSubmitURL(ctx, action, model) {
  const endpoint = KLING_NEW_ENDPOINTS[action];
  if (!endpoint) throw new Error("unsupported Kling new action: " + action);
  return apiRoot(ctx) + endpoint.path + "/" + encodeURIComponent(model);
}

function newQueryURL(ctx, taskId) {
  return apiRoot(ctx) + "/tasks?task_ids=" + encodeURIComponent(taskId);
}

function submitModel(ctx, req) {
  return (ctx && ctx.upstreamModel) ||
    (ctx && ctx.model) ||
    (req && (req.model_name || req.model)) ||
    "kling-v1";
}

function resolveKlingMode(model, mode) {
  const raw = trimmed(mode).toLowerCase();
  if (model === "kling-v2-master") {
    if (raw === "std") throw new Error("kling-v2-master does not support mode std");
    if (raw && raw !== "pro" && raw !== "4k") throw new Error("mode must be pro or 4k");
    return "pro";
  }
  if (!raw) return "std";
  if (raw !== "std" && raw !== "pro" && raw !== "4k") throw new Error("mode must be std, pro, or 4k");
  return raw;
}

function perSecondRate(model, mode) {
  const table = UNITS_PER_SECOND[model] || UNITS_PER_SECOND["kling-v1"];
  if (table[mode] !== undefined) return table[mode];
  if (table.pro !== undefined) return table.pro;
  return table.std;
}

function estimateUnits(model, mode, durationSeconds) {
  return perSecondRate(model, mode) * durationSeconds;
}

// Official current pages list 3–15s for new models; old-model duration "5"|"10"
// is unverifiable (research 2026-08-27). Keep permissive positive integers up to
// the host task duration bound.
function validateKlingDuration(value) {
  const n = Number(value);
  if (!Number.isInteger(n) || n <= 0 || n > 3600) throw new Error("seconds must be a positive integer at most 3600");
  return n;
}

function outboundDuration(req) {
  const n = Number(req && req.duration);
  if (Number.isFinite(n) && n > 0) return n;
  const metadata = (req && req.metadata) || {};
  const fromMeta = Number(metadata.duration);
  if (Number.isFinite(fromMeta) && fromMeta > 0) return fromMeta;
  return 5;
}

function newRequestSettings(req) {
  const settings = req && req.settings;
  if (settings === undefined) return {};
  if (typeof settings !== "object" || Array.isArray(settings)) throw new Error("settings must be an object");
  return settings;
}

function newReservedDuration(req, model, action) {
  const settings = newRequestSettings(req);
  if (action === "new_motion_control") {
    const orientation = trimmed(settings.character_orientation).toLowerCase();
    if (orientation !== "image" && orientation !== "video") throw new Error("settings.character_orientation must be image or video");
    return orientation === "image" ? 10 : MAX_NEW_OUTPUT_SECONDS;
  }
  const raw = Object.prototype.hasOwnProperty.call(settings, "duration") ? settings.duration : 5;
  const duration = Number(raw);
  if (!Number.isInteger(duration)) throw new Error("settings.duration must be an integer");
  if (model === "kling-3.0" || model === "kling-3.0-turbo" || model === "kling-3.0-omni") {
    if (duration < 3 || duration > 15) throw new Error("settings.duration must be between 3 and 15");
  } else if (model === "kling-o1") {
    if (duration < 3 || duration > 10) throw new Error("settings.duration must be between 3 and 10");
  } else if (duration !== 5 && duration !== 10) {
    throw new Error("settings.duration must be 5 or 10");
  }
  return duration;
}

function newOutputDuration(value) {
  const duration = Number(value);
  if (!Number.isFinite(duration) || duration <= 0 || duration > MAX_NEW_OUTPUT_SECONDS) return null;
  return duration;
}

function outboundMode(req, model) {
  const metadata = (req && req.metadata) || {};
  return resolveKlingMode(model, (req && req.mode) || metadata.mode);
}

function requestKlingOption(req, names) {
  const metadata = req && req.metadata && typeof req.metadata === "object" && !Array.isArray(req.metadata) ? req.metadata : {};
  const settings = req && req.settings && typeof req.settings === "object" && !Array.isArray(req.settings) ? req.settings : {};
  for (const source of [req || {}, metadata, settings]) {
    for (const name of names) {
      if (Object.prototype.hasOwnProperty.call(source, name)) return source[name];
    }
  }
  return undefined;
}

function klingAudioMode(req) {
  const value = requestKlingOption(req, ["sound", "audio", "generate_audio", "keep_original_sound"]);
  if (typeof value === "boolean") return value ? "sound" : "silent";
  const raw = String(value === undefined || value === null ? "" : value).trim().toLowerCase();
  return ["1", "true", "yes", "on", "sound", "audio", "enabled", "generate"].includes(raw) ? "sound" : "silent";
}

function hasKlingVideoReference(req, action) {
  if (action !== "omni_video") return false;
  const value = requestKlingOption(req, ["video_list", "videos", "video_url", "video", "input_video", "reference_video"]);
  if (Array.isArray(value)) return value.some(function (item) { return Boolean(trimmed(item && typeof item === "object" ? item.url || item.video_url : item)); });
  if (value && typeof value === "object" && !Array.isArray(value)) return Boolean(trimmed(value.url || value.video_url || value.uri));
  return Boolean(trimmed(value));
}

function isKlingDimensionPricedModel(model) {
  return model === "kling-v3" || model === "kling-v3-omni";
}

function klingDimensionUsage(ctx, req) {
  const model = submitModel(ctx, req);
  const action = ctx.action || "text_to_video";
  const mode = outboundMode(req, model);
  const facts = {
    seconds: outboundDuration(req),
    resolution: mode === "4k" ? "4k" : mode === "pro" ? "1080p" : "720p",
  };
  if (model === "kling-v3-omni") {
    facts.input_mode = hasKlingVideoReference(req, action) ? "reference_video" : klingAudioMode(req);
    return facts;
  }
  facts.operation = mode === "4k" ? "4k" : action === "motion_control" ? "motion_control" : "generation";
  facts.audio = klingAudioMode(req);
  return facts;
}

function hasKlingImage(req, hasInputReferenceFile) {
  if (hasInputReferenceFile) return true;
  const metadata = (req && req.metadata) || {};
  if (req && req.image && typeof req.image === "object" && !Array.isArray(req.image) && req.image.__fileRef) return true;
  return Boolean(trimmed(req && req.input_reference) || trimmed(req && req.image) || metadata.image || metadata.image_tail);
}

function filePlaceholder(image) {
  if (!image || typeof image !== "object" || Array.isArray(image) || !image.__fileRef) return image;
  const placeholder = { __fileRef: image.__fileRef, encoding: image.encoding };
  if (image.mimeType) placeholder.mimeType = image.mimeType;
  if (image.maxBytes !== undefined && image.maxBytes !== null) placeholder.maxBytes = image.maxBytes;
  return placeholder;
}

function decodeNativeSubmit(ctx) {
  if (!ctx.body || ctx.body.kind !== "json") throw new Error("JSON body required");
  const body = ctx.body.value;
  if (!body || typeof body !== "object" || Array.isArray(body)) throw new Error("request body must be an object");
  let model = typeof body.model_name === "string" ? body.model_name.trim() : "";
  if (model === "") model = typeof body.model === "string" ? body.model.trim() : "";
  if (model === "") model = trimmed(ctx.model) || trimmed(ctx.upstreamModel) || "kling-v1";
  if (!KLING_CLASSIC_MODELS.includes(model)) throw new Error("unsupported classic Kling model: " + model);
  return {
    kind: "submit",
    model: model,
    requestBody: Object.assign({}, body),
    action: ctx.action || "text_to_video",
  };
}

function decodeNewSubmit(ctx) {
  if (!ctx.body || ctx.body.kind !== "json") throw new Error("JSON body required");
  const body = ctx.body.value;
  if (!body || typeof body !== "object" || Array.isArray(body)) throw new Error("request body must be an object");
  const model = trimmed(ctx.params && ctx.params.model);
  const action = ctx.action;
  if (!isNewKlingAction(action)) throw new Error("unsupported Kling new action");
  if (!KLING_NEW_MODELS.includes(model)) throw new Error("unsupported Kling model: " + model);
  newReservedDuration(body, model, action);
  return { kind: "submit", model: model, action: action, requestBody: Object.assign({}, body) };
}

function publicTaskResponse(value, publicTaskId) {
  const result = value && typeof value === "object" && !Array.isArray(value) ? Object.assign({}, value) : {};
  if (result.data && typeof result.data === "object" && !Array.isArray(result.data)) {
    result.data = Object.assign({}, result.data);
    if (result.data.task_id) result.data.task_id = publicTaskId;
  }
  if (result.task_id) result.task_id = publicTaskId;
  return result;
}

function publicNewTaskResponse(value, publicTaskId) {
  const result = publicTaskResponse(value, publicTaskId);
  if (result.data && typeof result.data === "object" && !Array.isArray(result.data)) {
    result.data = Object.assign({}, result.data);
    if (result.data.id) result.data.id = publicTaskId;
  }
  return result;
}

function listQueryValue(query, names, fallback) {
  for (const name of names) {
    const values = query[name] || [];
    if (values.length > 1) throw new Error(name + " must be provided once");
    if (values.length === 1 && trimmed(values[0])) return values[0];
  }
  return fallback;
}

function endpointActionForPath(path) {
  const value = trimmed(path);
  for (const action of Object.keys(KLING_ENDPOINTS)) {
    if ("/kling" + KLING_ENDPOINTS[action].path === value) return action;
  }
  return "";
}

function splitNewTaskIDs(query, name) {
  const values = (query && query[name]) || [];
  if (values.length > 1) throw new Error(name + " must be provided once");
  if (values.length === 0 || !trimmed(values[0])) return [];
  const ids = String(values[0])
    .split(",")
    .map(function (value) {
      return trimmed(value);
    });
  if (ids.length > 100 || ids.some(function (value) { return value === "" || value.length > 191; })) throw new Error(name + " accepts at most 100 non-empty IDs");
  return ids;
}

function newTaskRecords(value) {
  const root = value && typeof value === "object" && !Array.isArray(value) ? value : {};
  if (Array.isArray(root.data)) return root.data;
  if (root.data && typeof root.data === "object" && !Array.isArray(root.data)) {
    if (Array.isArray(root.data.data)) return root.data.data;
    if (root.data.id || root.data.status || root.data.outputs) return [root.data];
  }
  if (root.id || root.status || root.outputs) return [root];
  return [];
}

function newOutputEntries(value) {
  const entries = [];
  for (const record of newTaskRecords(value)) {
    if (!record || typeof record !== "object" || !Array.isArray(record.outputs)) continue;
    for (const output of record.outputs) {
      if (!output || typeof output !== "object" || trimmed(output.type).toLowerCase() !== "video" || !trimmed(output.url)) continue;
      entries.push({ type: "video", url: trimmed(output.url), duration: newOutputDuration(output.duration) });
    }
  }
  return entries;
}

function newStatusForTask(status) {
  const statuses = {
    NOT_START: "submitted",
    SUBMITTED: "submitted",
    QUEUED: "submitted",
    IN_PROGRESS: "processing",
    SUCCESS: "succeeded",
    FAILURE: "failed",
  };
  return statuses[String(status || "").toUpperCase()] || "";
}

function taskViewMilliseconds(value) {
  const seconds = Number(value);
  if (!Number.isFinite(seconds) || seconds <= 0) return 0;
  return Math.floor(seconds * 1000);
}

function newTaskListItem(view) {
  const records = newTaskRecords(view && view.data);
  const publicTaskID = trimmed(view && view.task_id);
  let record = records.find(function (item) { return item && trimmed(item.id) === publicTaskID; });
  if (!record) record = records[0] || {};
  const item = Object.assign({}, record);
  if (publicTaskID) item.id = publicTaskID;
  const status = newStatusForTask(view && view.status);
  if (status) item.status = status;
  if (!item.message && trimmed(view && view.fail_reason)) item.message = trimmed(view.fail_reason);
  if (item.create_time === undefined) item.create_time = taskViewMilliseconds(view && view.created_at);
  if (item.update_time === undefined) item.update_time = taskViewMilliseconds(view && view.updated_at) || item.create_time;
  return item;
}

export const native = {
  decodeSubmit: decodeNativeSubmit,
  decodeNewSubmit: decodeNewSubmit,
  decodeProxy: function (ctx) {
    if (!ctx.body || (ctx.body.kind !== "none" && ctx.body.kind !== "json")) throw new Error("JSON or empty body required");
    const model = trimmed(ctx.model) || trimmed(ctx.upstreamModel) || "kling-v1";
    if (!KLING_CLASSIC_MODELS.includes(model)) throw new Error("unsupported classic Kling model: " + model);
    return { kind: "proxy", model: model, requestBody: ctx.body.kind === "json" ? ctx.body.value : { model: model } };
  },
  decodeTaskList: function (ctx) {
    if (!ctx.body || ctx.body.kind !== "none") throw new Error("request body is not allowed");
    const query = ctx.query || {};
    const model = trimmed(listQueryValue(query, ["filter.model", "model"], ""));
    if (model && !KLING_CLASSIC_MODELS.includes(model)) throw new Error("unsupported classic Kling model: " + model);
    const taskIds = query["filter.task_ids"] || query.task_ids || [];
    if (taskIds.length > 100) throw new Error("filter.task_ids accepts at most 100 task IDs");
    const pageNum = Number(listQueryValue(query, ["pageNum", "page_num"], 1));
    const pageSize = Number(listQueryValue(query, ["pageSize", "page_size"], 20));
    if (!Number.isInteger(pageNum) || pageNum < 1 || pageNum > 500 || !Number.isInteger(pageSize) || pageSize < 1 || pageSize > 500)
      throw new Error("pageNum and pageSize must be integers between 1 and 500");
    return {
      kind: "query",
      model: model,
      taskIds: taskIds.map(function (taskId) {
        return trimmed(taskId);
      }),
      actions: endpointActionForPath(ctx.path) ? [endpointActionForPath(ctx.path)] : [],
      listOptions: { pageNum: pageNum, pageSize: pageSize },
    };
  },
  decodeNewTaskList: function (ctx) {
    if (!ctx.body || ctx.body.kind !== "none") throw new Error("request body is not allowed");
    const query = ctx.query || {};
    const taskIds = splitNewTaskIDs(query, "task_ids");
    const externalTaskIds = splitNewTaskIDs(query, "external_task_ids");
    if (taskIds.length > 0 && externalTaskIds.length > 0) throw new Error("task_ids and external_task_ids cannot be used together");
    return {
      kind: "query",
      taskIds: taskIds,
      externalTaskIds: externalTaskIds,
      actions: KLING_NEW_ACTIONS,
      listOptions: { pageNum: 1, pageSize: 100 },
    };
  },
  taskCreated: function (ctx, task) {
    return publicTaskResponse(task.data, task.task_id);
  },
  newTaskCreated: function (ctx, task) {
    return publicNewTaskResponse(task.data, task.task_id);
  },
  taskStatus: function (ctx, task) {
    if (task.data && typeof task.data === "object" && !Array.isArray(task.data)) {
      return publicTaskResponse(task.data, task.task_id);
    }
    const statusMap = { NOT_START: "submitted", SUBMITTED: "submitted", QUEUED: "submitted", IN_PROGRESS: "processing", SUCCESS: "succeed", FAILURE: "failed" };
    return { code: 0, data: { task_id: task.task_id, task_status: statusMap[task.status] || "submitted", task_status_msg: task.fail_reason || "" } };
  },
  taskList: function (ctx, result) {
    return {
      items: Array.isArray(result && result.items) ? result.items : [],
      total: Number(result && result.total) || 0,
      page_num: Number(result && result.page_num) || 1,
      page_size: Number(result && result.page_size) || 20,
    };
  },
  newTaskList: function (ctx, result) {
    const items = Array.isArray(result && result.items) ? result.items : [];
    let requestID = "";
    const data = items.map(function (item) {
      if (!requestID && item && item.data && trimmed(item.data.request_id)) requestID = trimmed(item.data.request_id);
      return newTaskListItem(item);
    });
    return { code: 0, message: "", request_id: requestID, data: data };
  },
  proxyResponse: function (ctx, response) {
    return response.body;
  },
  error: function (ctx, error) {
    return { code: error.code, message: error.message };
  },
};

export function buildNativeRequest(ctx) {
  const action = endpointActionForPath(ctx.path);
  if (!action) throw new Error("unsupported Kling proxy path");
  const method = String(ctx.method || "GET").toUpperCase();
  const descriptor = {
    url: urlFor(ctx, action),
    method: method,
    headers: { Accept: "application/json", Authorization: "Bearer " + tokenFor(ctx), "User-Agent": "kling-sdk/1.0" },
  };
  if (method !== "GET" && ctx.requestBody !== undefined && ctx.requestBody !== null) {
    descriptor.body = ctx.requestBody;
    descriptor.headers["Content-Type"] = "application/json";
  }
  return descriptor;
}

export function buildSubmitRequest(ctx) {
  const req = ctx.requestBody && typeof ctx.requestBody === "object" && !Array.isArray(ctx.requestBody) ? ctx.requestBody : {};
  if (isNewKlingAction(ctx.action)) {
    const model = submitModel(ctx, req);
    newReservedDuration(req, model, ctx.action);
    return {
      url: newSubmitURL(ctx, ctx.action, model),
      method: "POST",
      headers: { "Content-Type": "application/json", Accept: "application/json", Authorization: "Bearer " + tokenFor(ctx), "User-Agent": "kling-sdk/1.0" },
      body: Object.assign({}, req),
      action: ctx.action,
    };
  }
  const action = KLING_ENDPOINTS[ctx.action] ? ctx.action : req.image || req.image_url ? "image_to_video" : "text_to_video";
  const model = submitModel(ctx, req);
  const body = Object.assign({}, req);
  const metadata = body.metadata && typeof body.metadata === "object" && !Array.isArray(body.metadata) ? body.metadata : null;
  if (metadata) {
    delete body.metadata;
    Object.assign(body, metadata);
  }
  if (!body.model_name && KLING_MODEL_ACTIONS.includes(action)) body.model_name = model;
  if (body.image) body.image = filePlaceholder(body.image);
  if (body.image_tail) body.image_tail = filePlaceholder(body.image_tail);
  if (body.image_url) body.image_url = filePlaceholder(body.image_url);
  if (body.sound_file) body.sound_file = filePlaceholder(body.sound_file);
  if (body.video_url) body.video_url = filePlaceholder(body.video_url);
  if (body.normalized_video) body.normalized_video = filePlaceholder(body.normalized_video);
  if (body.voice_url) body.voice_url = filePlaceholder(body.voice_url);
  if (body.input && typeof body.input === "object") body.input = filePlaceholder(body.input);
  return {
    url: urlFor(ctx, action),
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json", Authorization: "Bearer " + tokenFor(ctx), "User-Agent": "kling-sdk/1.0" },
    body: body,
    action: action,
  };
}

export function parseSubmitResponse(ctx, resp) {
  const result = resp.body || {};
  if (!result || typeof result !== "object" || Array.isArray(result)) throw new Error("invalid Kling response");
  if (result.code !== undefined && result.code !== null && String(result.code) !== "0") throw new Error(result.message || "kling submit failed");
  const data = result.data && typeof result.data === "object" && !Array.isArray(result.data) ? result.data : {};
  if (isNewKlingAction(ctx.action)) {
    const newTaskID = trimmed(data.id);
    if (!newTaskID) throw new Error("missing task id");
    return { taskId: newTaskID, taskData: result };
  }
  const taskId = trimmed(data.task_id || result.task_id || ctx.publicTaskId);
  if (!taskId) throw new Error("missing task_id");
  if (data.task_id) return { taskId: taskId, taskData: result };
  return { taskId: taskId, taskData: result, immediate: { status: "SUCCESS", progress: "100%" } };
}

export function extractUsage(ctx) {
  if (ctx.usagePurpose === "billing_ratios") return null;
  const req = ctx.requestBody || {};
  const model = submitModel(ctx, req);
  if (isKlingDimensionPricedModel(model)) return klingDimensionUsage(ctx, req);
  if (KLING_SYNC_ACTIONS.includes(ctx.action)) return { units: 0 };
  if (isNewKlingAction(ctx.action)) {
    return { units: estimateUnits(model, "std", newReservedDuration(req, model, ctx.action)) };
  }
  if (!KLING_VIDEO_METERED_ACTIONS.includes(ctx.action)) return { units: 1 };
  const duration = outboundDuration(req);
  const mode = outboundMode(req, model);
  return { units: estimateUnits(model, mode, duration) };
}

export function buildQueryRequest(ctx) {
  if (isNewKlingAction(ctx.action)) {
    return {
      url: newQueryURL(ctx, ctx.taskId),
      method: "GET",
      headers: { Accept: "application/json", Authorization: "Bearer " + tokenFor(ctx), "User-Agent": "kling-sdk/1.0" },
    };
  }
  return {
    url: urlFor(ctx, ctx.action) + "/" + ctx.taskId,
    method: "GET",
    headers: { Accept: "application/json", Authorization: "Bearer " + tokenFor(ctx), "User-Agent": "kling-sdk/1.0" },
  };
}

export function parseTaskResult(ctx, body) {
  if (isNewKlingAction(ctx.action)) return parseNewTaskResult(ctx, body);
  if (body && body.code !== undefined && body.code !== null && String(body.code) !== "0") throw new Error(body.message || "kling query failed");
  const data = body && body.data && typeof body.data === "object" ? body.data : {};
  const rawStatus = trimmed(data.task_status).toLowerCase();
  const statuses = { submitted: "SUBMITTED", queued: "QUEUED", processing: "IN_PROGRESS", in_progress: "IN_PROGRESS", succeed: "SUCCESS", succeeded: "SUCCESS", failed: "FAILURE", cancelled: "FAILURE", canceled: "FAILURE" };
  const status = statuses[rawStatus];
  if (!status) return { status: "UNKNOWN", reason: "unknown task status: " + String(data.task_status || "") };
  const result = { code: body.code || 0, taskId: data.task_id || ctx.taskId, status: status, reason: data.task_status_msg || "" };
  const taskResult = data.task_result && typeof data.task_result === "object" ? data.task_result : {};
  const media = [];
  for (const key of ["videos", "audios", "images"]) {
    if (Array.isArray(taskResult[key])) media.push.apply(media, taskResult[key]);
  }
  if (status === "SUCCESS" && media.length && media[0] && media[0].url) result.url = media[0].url;
  const units = Number.parseFloat(data.final_unit_deduction || "");
  if (Number.isFinite(units) && units >= 0 && units <= MAX_UPSTREAM_UNITS) {
    result.completionTokens = Math.ceil(units);
    result.totalTokens = Math.ceil(units);
  }
  return result;
}

function parseNewTaskResult(ctx, body) {
  if (body && body.code !== undefined && body.code !== null && String(body.code) !== "0") throw new Error(body.message || "kling query failed");
  const records = newTaskRecords(body);
  const record = records.find(function (item) { return item && trimmed(item.id) === trimmed(ctx.taskId); }) || (records.length === 1 ? records[0] : null);
  if (!record) return { status: "UNKNOWN", reason: "Kling task was not returned by the query endpoint" };
  const statusMap = { submitted: "SUBMITTED", processing: "IN_PROGRESS", succeeded: "SUCCESS", failed: "FAILURE" };
  const rawStatus = trimmed(record.status).toLowerCase();
  const status = statusMap[rawStatus];
  if (!status) return { status: "UNKNOWN", reason: "unknown Kling task status: " + String(record.status || "") };
  const result = { taskId: ctx.taskId, status: status, reason: status === "FAILURE" ? trimmed(record.message) : "" };
  if (status === "SUCCESS") {
    const output = newOutputEntries({ data: [record] })[0];
    if (output) result.url = output.url;
  }
  return result;
}

function taskPayload(value) {
  const data = (value && typeof value === "object" && !Array.isArray(value) ? value : {}) || {};
  if (data.data && typeof data.data === "object" && !Array.isArray(data.data)) {
    const nested = data.data;
    if (nested.task_id || nested.task_status || nested.task_result || nested.final_unit_deduction !== undefined) return nested;
    if (nested.data && typeof nested.data === "object" && !Array.isArray(nested.data)) return nested.data;
  }
  return data;
}

function artifactEntries(value) {
  const result = taskPayload(value).task_result;
  const entries = [];
  if (result && typeof result === "object" && !Array.isArray(result)) {
    for (const key of ["videos", "audios", "images"]) {
      if (!Array.isArray(result[key])) continue;
      for (const item of result[key]) {
        if (item && typeof item === "object" && trimmed(item.url)) entries.push({ type: key === "videos" ? "video" : key === "audios" ? "audio" : "image", url: trimmed(item.url), duration: key === "videos" ? newOutputDuration(item.duration) : null });
      }
    }
  }
  return entries.concat(newOutputEntries(value));
}

export function listArtifacts(task) {
  if (task.status !== "SUCCESS") return [];
  const counters = {};
  return artifactEntries(task).map(function (entry) {
    counters[entry.type] = (counters[entry.type] || 0) + 1;
    const key = entry.type === "video" && counters[entry.type] === 1 ? "video" : entry.type + "-" + counters[entry.type];
    return { key: key, type: entry.type, mimeType: entry.type === "video" ? "video/mp4" : entry.type === "audio" ? "audio/mpeg" : "image/png" };
  });
}

export function buildContentRequest(ctx) {
  const entries = artifactEntries(ctx.data);
  let entry;
  if (ctx.artifactKey === "video") entry = entries.find(function (item) { return item.type === "video"; });
  else {
    const match = /^(video|audio|image)-(\d+)$/.exec(String(ctx.artifactKey || ""));
    if (match) {
      const type = match[1], index = Number(match[2]) - 1;
      entry = entries.filter(function (item) { return item.type === type; })[index];
    }
  }
  const url = entry ? entry.url : "";
  if (!url) throw new Error("artifact_not_found");
  return { url: url, method: ctx.clientRequest.method, credentialless: true };
}

export function extractUsageOnComplete(_task, _taskResult, body) {
  const task = _task || {};
  const completedModel = trimmed(task.upstreamModel) || trimmed(task.model);
  if (isKlingDimensionPricedModel(completedModel)) {
    const data = taskPayload(body);
    const taskResult = data && data.task_result && typeof data.task_result === "object" && !Array.isArray(data.task_result) ? data.task_result : {};
    const videos = Array.isArray(taskResult.videos) ? taskResult.videos : [];
    const seconds = Number(videos[0] && videos[0].duration);
    if (Number.isFinite(seconds) && seconds > 0 && seconds <= 3600) return { seconds: seconds };
    return null;
  }
  if (isNewKlingAction(task.action)) {
    const output = newOutputEntries(body)[0];
    const outputDuration = output && output.duration;
    if (outputDuration === null || outputDuration === undefined) return null;
    const model = trimmed(task.upstreamModel) || trimmed(task.model);
    if (!model) return null;
    return { units: estimateUnits(model, "std", outputDuration) };
  }
  const data = taskPayload(body);
  if (
    !Object.prototype.hasOwnProperty.call(data, "final_unit_deduction") ||
    data.final_unit_deduction === undefined ||
    data.final_unit_deduction === null ||
    data.final_unit_deduction === ""
  ) {
    return null;
  }
  const units = Number.parseFloat(data.final_unit_deduction);
  if (!Number.isFinite(units) || units < 0 || units > MAX_UPSTREAM_UNITS) return null;
  return { units: units };
}

function newOpenAIReference(value) {
  if (value && typeof value === "object" && !Array.isArray(value) && value.__fileRef) return value;
  const text = trimmed(value);
  return text || "";
}

function buildNewOpenAIRequest(model, prompt, images, metadata, req) {
  const references = (images || []).map(newOpenAIReference).filter(function (value) {
    return (value && typeof value === "object") || Boolean(value);
  });
  const omni = model === "kling-3.0-omni" || model === "kling-o1";
  if (!omni && references.length > 2) throw new Error("new Kling image-to-video accepts at most two images");
  const contents = [];
  if (trimmed(prompt)) contents.push({ type: "prompt", text: trimmed(prompt) });
  if (references.length) contents.push({ type: "first_frame", url: references[0] });
  if (references.length > 1) contents.push({ type: "last_frame", url: references[1] });
  if (omni) {
    for (let index = 2; index < references.length; index += 1) contents.push({ type: "refer_image", url: references[index], id: "image_" + (index + 1) });
  }
  const sourceSettings = metadata && metadata.settings && typeof metadata.settings === "object" && !Array.isArray(metadata.settings) ? metadata.settings : {};
  const settings = Object.assign({}, sourceSettings);
  for (const key of ["resolution", "aspect_ratio", "duration", "audio", "multi_shot", "character_orientation"]) {
    if (metadata && Object.prototype.hasOwnProperty.call(metadata, key)) settings[key] = metadata[key];
  }
  if (req && Object.prototype.hasOwnProperty.call(req, "seconds")) settings.duration = req.seconds;
  else if (req && Object.prototype.hasOwnProperty.call(req, "duration")) settings.duration = req.duration;
  if (req && Object.prototype.hasOwnProperty.call(req, "size")) settings.resolution = req.size;
  const options = metadata && metadata.options && typeof metadata.options === "object" && !Array.isArray(metadata.options) ? metadata.options : {};
  return {
    action: omni ? "new_omni_video" : references.length ? "new_image_to_video" : "new_text_to_video",
    requestBody: { contents: contents, settings: settings, options: Object.assign({}, options) },
  };
}

export const protocols = {
  openai_responses: {
    decodeRequest: function (ctx) {
      if (!ctx.body || ctx.body.kind !== "json") throw new Error("JSON body required");
      const req = ctx.body.value;
      if (!req || typeof req !== "object" || Array.isArray(req)) throw new Error("request body must be an object");
      const model = trimmed(ctx.model);
      if (!model) throw new Error("model is required");
      if (req.input !== undefined && typeof req.input !== "string" && !Array.isArray(req.input)) throw new Error("input must be a string or array");
      if (req.images !== undefined && !Array.isArray(req.images)) throw new Error("images must be an array");
      if (req.metadata !== undefined && (!req.metadata || typeof req.metadata !== "object" || Array.isArray(req.metadata)))
        throw new Error("metadata must be an object");
      const input = responsesInput(req);
      const prompt = input.prompt || trimmed(req.prompt);
      const images = [];
      for (const image of [req.image, req.input_reference].concat(req.images || [], input.images)) {
        if (trimmed(image) && !images.includes(trimmed(image))) images.push(trimmed(image));
      }
      if (!prompt && images.length === 0) throw new Error("input is required");
      const metadata = Object.assign({}, req.metadata || {});
      const routedModel = trimmed(ctx.upstreamModel) || model;
      if (KLING_NEW_MODELS.includes(routedModel)) {
        const converted = buildNewOpenAIRequest(routedModel, prompt, images, metadata, req);
        return { kind: "submit", model: model, action: converted.action, requestBody: converted.requestBody };
      }
      if (Object.prototype.hasOwnProperty.call(req, "mode")) metadata.mode = req.mode;
      metadata.mode = resolveKlingMode(ctx.upstreamModel || model, metadata.mode);
      if (images.length > 1 && !metadata.image_tail) metadata.image_tail = images[1];
      const requestBody = { model: model, prompt: prompt, metadata: metadata };
      if (images.length) requestBody.image = images[0];
      if (Object.prototype.hasOwnProperty.call(req, "seconds")) requestBody.duration = req.seconds;
      else if (Object.prototype.hasOwnProperty.call(req, "duration")) requestBody.duration = req.duration;
      if (Object.prototype.hasOwnProperty.call(req, "size")) requestBody.size = req.size;
      return { kind: "submit", model: model, action: images.length ? "image_to_video" : "text_to_video", requestBody: requestBody };
    },
    renderEvents: function (ctx, task, previousState) {
      const status = String(task.status || "UNKNOWN").toUpperCase();
      const value = Number(String(task.progress || "").replace("%", ""));
      const progress = Number.isFinite(value) && value >= 0 && value <= 100 ? value : null;
      const state = { status: status, progress: progress };
      if (status === "SUCCESS") {
        const text = responsesVideoText(ctx);
        const events = previousState && previousState.status === status ? [] : [{ type: "output", data: text }];
        return { events: events, state: state, done: true };
      }
      if (status === "FAILURE")
        return { events: [{ type: "error", code: "task_failed", message: task.fail_reason || "task failed" }], state: state, done: true };
      if (previousState && previousState.status === status && previousState.progress === progress) return { events: [], state: state, done: false };
      const event = { type: "progress", message: status.toLowerCase() };
      if (progress !== null) event.progress = progress;
      return { events: [event], state: state, done: false };
    },
    renderFinal: function (ctx, _task) {
      return {
        output: [
          {
            type: "message",
            status: "completed",
            role: "assistant",
            content: [{ type: "output_text", text: responsesVideoText(ctx), annotations: [], logprobs: [] }],
          },
        ],
        metadata: { vendor: "kling" },
      };
    },
  },
  openai_video: {
    decodeRequest: function (ctx) {
      if (!ctx.body || (ctx.body.kind !== "json" && ctx.body.kind !== "multipart")) throw new Error("JSON or multipart body required");
      let req;
      let hasInputReferenceFile = false;
      if (ctx.body.kind === "json") {
        if (!ctx.body.value || Array.isArray(ctx.body.value)) throw new Error("JSON object required");
        req = Object.assign({}, ctx.body.value);
      } else {
        const first = function (name) {
          const values = (ctx.body.fields || {})[name] || [];
          if (values.length > 1) throw new Error(name + " must be provided once");
          return values[0];
        };
        req = {};
        const fields = ctx.body.fields || {};
        for (const name of Object.keys(fields)) {
          req[name] = first(name);
        }
        for (const file of ctx.body.files || []) {
          if (file.field !== "input_reference") throw new Error("unexpected file field: " + file.field);
          if (hasInputReferenceFile) throw new Error("input_reference must be provided once");
          hasInputReferenceFile = true;
        }
        if (req.metadata !== undefined) {
          let parsed;
          try {
            parsed = JSON.parse(req.metadata);
          } catch (e) {
            throw new Error("metadata must be a JSON object string", { cause: e });
          }
          if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error("metadata must be a JSON object string");
          req.metadata = parsed;
        }
        if (req.seconds !== undefined) req.seconds = Number(req.seconds);
        else if (req.duration !== undefined) req.seconds = Number(req.duration);
      }
      const seconds = req.seconds === undefined ? req.duration : req.seconds;
      if (seconds !== undefined) req.duration = validateKlingDuration(seconds);
      else req.duration = 5;
      if (hasInputReferenceFile) {
        req.image = { __fileRef: "request_file:input_reference", encoding: "base64", maxBytes: 10485760 };
      } else {
        const image = trimmed(req.input_reference || req.image);
        if (image) req.image = image;
      }
      const model = ctx.upstreamModel || ctx.model || req.model || "kling-v1";
      const metadata = req.metadata || {};
      if (KLING_NEW_MODELS.includes(model)) {
        const references = hasInputReferenceFile || req.image ? [req.image] : [];
        const converted = buildNewOpenAIRequest(model, req.prompt, references, metadata, req);
        newReservedDuration(converted.requestBody, model, converted.action);
        return {
          kind: "submit",
          model: ctx.model || model,
          action: converted.action,
          requestBody: converted.requestBody,
        };
      }
      req.mode = resolveKlingMode(model, req.mode || metadata.mode);
      const hasImage = hasKlingImage(req, hasInputReferenceFile);
      return {
        kind: "submit",
        model: ctx.model,
        action: hasImage ? "image_to_video" : "text_to_video",
        requestBody: Object.assign({}, req, { model: ctx.model }),
      };
    },
    render: function (ctx, task) {
      const response = task.data || {};
      const data = response.data || {};
      const statusMap = { NOT_START: "queued", SUBMITTED: "queued", QUEUED: "queued", IN_PROGRESS: "in_progress", SUCCESS: "completed", FAILURE: "failed" };
      const output = {
        id: task.task_id,
        object: "video",
        model: "",
        status: statusMap[task.status] || "unknown",
        progress: Number(String(task.progress || "0").replace("%", "")),
        created_at: data.created_at || 0,
      };
      if (data.updated_at) output.completed_at = data.updated_at;
      const videos = artifactEntries(task).filter(function (entry) { return entry.type === "video"; });
      if (videos.length && videos[0].duration) output.seconds = videos[0].duration;
      if (response.code !== 0 && response.message) output.error = { message: response.message, code: String(response.code) };
      if (data.task_status === "failed") output.error = { message: data.task_status_msg, code: "" };
      if (data && Array.isArray(response.data)) {
        const record = newTaskRecords(response).find(function (item) { return item && String(item.id || "") === String(task.task_id || ""); });
        if (record && record.message) output.error = { message: record.message, code: "" };
      }
      return output;
    },
  },
};
