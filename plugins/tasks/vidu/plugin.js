const VIDU_MODELS = [
  "viduq3-pro",
  "viduq3-turbo",
  "viduq3-pro-fast",
  "viduq3-mix",
  "viduq3",
  "viduq2-pro-fast",
  "viduq2-pro",
  "viduq2-turbo",
  "viduq2",
  "viduq1",
  "viduq1-classic",
  "vidu2.0",
  "vidu1.5",
];

// 模板接口没有 model 字段，使用旧插件已有的 q2 名称选择渠道，避免升级后已有渠道无法承载模板任务。
const VIDU_DEFAULT_NATIVE_MODEL = "viduq2";
const RESOLUTIONS = ["360p", "540p", "720p", "1080p"];
const VIDU_Q3_PRO_RESOLUTIONS = ["720p", "1080p"];
const VIDU_Q3_TURBO_RESOLUTIONS = ["540p", "720p", "1080p"];
const VIDU_ACTIONS = ["text_to_video", "image_to_video", "reference_to_video", "first_tail_to_video", "multiframe", "template", "template_story"];

const VIDU_DURATION_FIELD = {
  type: "number",
  unit: "second",
  description: { en: "Video generation unit price", zh: "视频生成单价" },
};

function viduUsageSchema(resolutions) {
  const enumLabels = {};
  for (const resolution of resolutions) enumLabels[resolution] = { en: resolution, zh: resolution };
  return {
    duration: VIDU_DURATION_FIELD,
    resolution: {
      enum: resolutions,
      enumLabels: enumLabels,
      description: { en: "Output video resolution", zh: "输出视频分辨率" },
    },
  };
}

function viduQ3UsageExamples(resolutions) {
  return resolutions.map(function (resolution) {
    return { label: "Q3 " + resolution + " 5s", facts: { duration: 5, resolution: resolution } };
  });
}

export const meta = {
  apiVersion: 1,
  key: "vidu",
  name: "Vidu",
  icon: "Vidu.Color",
  description: {
    en: "Shengshu Vidu video generation and template workflows",
    zh: "生数 Vidu 视频生成及模板工作流",
  },
  version: "1.1.0",
  author: { name: "QuantumNous" },
  channelTypes: [52],
  upstreams: ["vendor", "new_api"],
  models: VIDU_MODELS,
  fetchMode: "per_task",
  usageSchema: viduUsageSchema(RESOLUTIONS),
  usageExamples: [
    { label: "q2 5s 720p", facts: { duration: 5, resolution: "720p" } },
    { label: "q1 5s 1080p", facts: { duration: 5, resolution: "1080p" } },
    { label: "2.0 4s 360p", facts: { duration: 4, resolution: "360p" } },
    { label: "2.0 4s 720p", facts: { duration: 4, resolution: "720p" } },
    { label: "2.0 8s 720p", facts: { duration: 8, resolution: "720p" } },
  ],
  usageProfiles: [
    { models: ["viduq3-pro"], schema: viduUsageSchema(VIDU_Q3_PRO_RESOLUTIONS), examples: viduQ3UsageExamples(VIDU_Q3_PRO_RESOLUTIONS) },
    { models: ["viduq3-turbo"], schema: viduUsageSchema(VIDU_Q3_TURBO_RESOLUTIONS), examples: viduQ3UsageExamples(VIDU_Q3_TURBO_RESOLUTIONS) },
  ],
  routes: viduRoutes(),
  protocols: [{ name: "openai_responses", supports: ["stream", "sync", "background"] }, "openai_video"],
};

function viduRoutes() {
  return [
    { method: "POST", path: "/vidu/ent/v2/text2video", type: "submit", action: "text_to_video", decode: "createTextVideoTask", render: "viduTaskCreated" },
    { method: "POST", path: "/vidu/ent/v2/img2video", type: "submit", action: "image_to_video", decode: "createImageVideoTask", render: "viduTaskCreated" },
    { method: "POST", path: "/vidu/ent/v2/reference2video", type: "submit", action: "reference_to_video", decode: "createReferenceVideoTask", render: "viduTaskCreated" },
    { method: "POST", path: "/vidu/ent/v2/start-end2video", type: "submit", action: "first_tail_to_video", decode: "createFirstTailVideoTask", render: "viduTaskCreated" },
    { method: "POST", path: "/vidu/ent/v2/multiframe", type: "submit", action: "multiframe", decode: "createMultiframeTask", render: "viduTaskCreated" },
    { method: "POST", path: "/vidu/ent/v2/template", type: "submit", action: "template", decode: "createTemplateTask", render: "viduTaskCreated" },
    { method: "POST", path: "/vidu/ent/v2/template-story", type: "submit", action: "template_story", decode: "createTemplateStoryTask", render: "viduTaskCreated" },
    { method: "GET", path: "/vidu/ent/v2/tasks", type: "dynamic", action: "list", decode: "decodeViduTaskList", render: "viduTaskList" },
  ];
}

function trimmed(value) {
  return String(value || "").trim();
}

function viaGateway(ctx) {
  return !!(ctx && ctx.upstream && ctx.upstream.kind === "new_api");
}

function apiRoot(ctx) {
  const base = trimmed(ctx && ctx.baseUrl).replace(/\/+$/, "");
  if (base.toLowerCase().endsWith("/vidu")) return base;
  return base + (viaGateway(ctx) ? "/vidu" : "");
}

function isQ2Model(model) {
  return String(model || "").indexOf("viduq2") === 0;
}

function isQ3Model(model) {
  return String(model || "").indexOf("viduq3") === 0;
}

function isQ1Model(model) {
  return model === "viduq1" || model === "viduq1-classic";
}

function supportedResolutions(model) {
  if (model === "viduq3-pro") return VIDU_Q3_PRO_RESOLUTIONS;
  if (model === "viduq3-turbo") return VIDU_Q3_TURBO_RESOLUTIONS;
  return RESOLUTIONS;
}

function defaultDuration(model) {
  if (model === "vidu2.0") return 4;
  return 5;
}

function defaultResolution(model) {
  if (isQ2Model(model) || isQ3Model(model)) return "720p";
  if (model === "vidu2.0") return "360p";
  return "1080p";
}

function normalizeResolution(value, model) {
  if (isQ1Model(model)) return "1080p";
  const supported = supportedResolutions(model);
  const raw = trimmed(value).toLowerCase();
  if (RESOLUTIONS.indexOf(raw) >= 0) {
    if (supported.indexOf(raw) < 0) throw new Error(model + " does not support " + raw + " resolution; supported resolutions: " + supported.join(", "));
    return raw;
  }
  const parts = raw.replace("*", "x").split("x");
  if (parts.length === 2) {
    const width = Number(parts[0]);
    const height = Number(parts[1]);
    if (width > 0 && height > 0) {
      const max = Math.max(width, height);
      const normalized = max >= 1920 ? "1080p" : max >= 1280 ? "720p" : max >= 960 ? "540p" : "360p";
      if (supported.indexOf(normalized) < 0) throw new Error(model + " does not support " + normalized + " resolution; supported resolutions: " + supported.join(", "));
      return normalized;
    }
  }
  return defaultResolution(model);
}

function outboundDuration(req, model) {
  const n = Number(req && req.duration);
  if (Number.isFinite(n) && n > 0) return n;
  return defaultDuration(model);
}

function multiframeDuration(req) {
  const settings = req && req.image_settings;
  if (!Array.isArray(settings) || settings.length === 0) return null;
  let total = 0;
  for (const item of settings) {
    const value = Number(item && item.duration);
    total += Number.isFinite(value) && value > 0 ? value : 5;
  }
  return total > 0 ? total : null;
}

function outboundDurationForAction(req, model, action) {
  if (action === "multiframe") {
    const total = multiframeDuration(req);
    if (total !== null) return total;
  }
  return outboundDuration(req, model);
}

function outboundResolution(req, model) {
  if (req && req.resolution) return normalizeResolution(req.resolution, model);
  const metadata = (req && req.metadata) || {};
  if (metadata.resolution) return normalizeResolution(metadata.resolution, model);
  if (req && req.size) return normalizeResolution(req.size, model);
  return defaultResolution(model);
}

function hasViduImages(req, hasInputReferenceFile) {
  if (hasInputReferenceFile) return true;
  if (Array.isArray(req && req.images) && req.images.length) return true;
  return Boolean(trimmed(req && req.input_reference) || trimmed(req && req.image));
}

function validateViduCombo(model, duration, resolution, hasImages) {
  if (isQ3Model(model) && resolution && supportedResolutions(model).indexOf(resolution) < 0)
    throw new Error(model + " does not support " + resolution + " resolution; supported resolutions: " + supportedResolutions(model).join(", "));
  if (model === "vidu2.0" && !hasImages) {
    throw new Error("vidu2.0 does not support text-to-video");
  }
  if (isQ1Model(model)) {
    if (duration !== undefined && Number(duration) !== 5) throw new Error("viduq1 duration must be 5");
    return;
  }
  if (model === "vidu2.0") {
    const n = duration === undefined ? 4 : Number(duration);
    if (n === 4) {
      if (["360p", "720p", "1080p"].indexOf(resolution) < 0) throw new Error("vidu2.0 duration 4 only allows resolution 360p, 720p, or 1080p");
      return;
    }
    if (n === 8) {
      if (resolution !== "720p") throw new Error("vidu2.0 duration 8 only allows resolution 720p");
      return;
    }
    throw new Error("vidu2.0 duration must be 4 or 8");
  }
  if (isQ2Model(model)) {
    if (duration === undefined) return;
    const n = Number(duration);
    if (!Number.isInteger(n) || n < 1 || n > 10) throw new Error("viduq2 duration must be between 1 and 10");
    return;
  }
  if (isQ3Model(model)) {
    if (duration === undefined) return;
    const n = Number(duration);
    if (!Number.isInteger(n) || n < 1 || n > 16) throw new Error("viduq3 duration must be between 1 and 16");
    return;
  }
  if (duration !== undefined) {
    const n = Number(duration);
    if (!Number.isInteger(n) || n <= 0 || n > 3600) throw new Error("seconds must be between 1 and 3600");
  }
}

function filePlaceholder(image) {
  if (!image || typeof image !== "object" || Array.isArray(image) || !image.__fileRef) return image;
  const placeholder = { __fileRef: image.__fileRef, encoding: image.encoding };
  if (image.mimeType) placeholder.mimeType = image.mimeType;
  if (image.maxBytes !== undefined && image.maxBytes !== null) placeholder.maxBytes = image.maxBytes;
  return placeholder;
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

function actionFor(req) {
  if (req.metadata && req.metadata.action) {
    const aliases = {
      generate: "image_to_video",
      textGenerate: "text_to_video",
      firstTailGenerate: "first_tail_to_video",
      referenceGenerate: "reference_to_video",
      remixGenerate: "remix",
    };
    return aliases[req.metadata.action] || req.metadata.action;
  }
  if (!req.images || req.images.length === 0) return "text_to_video";
  if (req.images.length === 2) return "first_tail_to_video";
  if (req.images.length > 2) return "reference_to_video";
  return "image_to_video";
}

function normalizedAction(req, preferred) {
  if (VIDU_ACTIONS.includes(preferred)) return preferred;
  return actionFor(req);
}

function pathFor(action) {
  if (action === "image_to_video") return "/img2video";
  if (action === "first_tail_to_video") return "/start-end2video";
  if (action === "reference_to_video") return "/reference2video";
  if (action === "multiframe") return "/multiframe";
  if (action === "template") return "/template";
  if (action === "template_story") return "/template-story";
  return "/text2video";
}

export function buildSubmitRequest(ctx) {
  const req = ctx.requestBody && typeof ctx.requestBody === "object" && !Array.isArray(ctx.requestBody) ? ctx.requestBody : {};
  const action = normalizedAction(req, ctx.action);
  const metadata = req.metadata && typeof req.metadata === "object" && !Array.isArray(req.metadata) ? req.metadata : {};
  let model = ctx.upstreamModel || req.model || VIDU_DEFAULT_NATIVE_MODEL;
  if (action !== "template" && action !== "template_story") {
    const duration = req.duration === undefined ? undefined : Number(req.duration);
    validateViduCombo(model, duration, outboundResolution(req, model), action !== "text_to_video");
  }
  const body = Object.assign({}, req, metadata);
  const images = Array.isArray(body.images) ? body.images.map(filePlaceholder) : null;
  if (images) body.images = images;
  delete body.metadata;
  delete body.action;
  if (action !== "template" && action !== "template_story") body.model = model;
  else delete body.model;
  if (action !== "multiframe" && action !== "template" && action !== "template_story") {
    if (body.prompt === undefined && req.prompt !== undefined) body.prompt = req.prompt;
    if (body.duration === undefined) body.duration = outboundDuration(req, model);
    if (body.resolution === undefined) body.resolution = outboundResolution(req, model);
    if (body.movement_amplitude === undefined) body.movement_amplitude = "auto";
  } else if (action === "multiframe" && body.resolution === undefined) {
    body.resolution = outboundResolution(req, model);
  }
  return {
    url: apiRoot(ctx) + "/ent/v2" + pathFor(action),
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json", Authorization: "Bearer " + ctx.apiKey },
    body: body,
    action: action,
  };
}

export function parseSubmitResponse(ctx, resp) {
  const body = resp && resp.body && typeof resp.body === "object" && !Array.isArray(resp.body) ? resp.body : {};
  const error = viduErrorMessage(body);
  if (error) throw new Error(error);
  if (body.state === "failed") throw new Error(viduFailureReason(body));
  if (!trimmed(body.task_id)) throw new Error("missing task_id");
  return { taskId: body.task_id, taskData: body };
}

export function extractUsage(ctx) {
  const req = ctx.requestBody || {};
  const model = ctx.upstreamModel || req.model;
  const action = normalizedAction(req, ctx.action);
  return { duration: outboundDurationForAction(req, model, action), resolution: outboundResolution(req, model) };
}

export function buildQueryRequest(ctx) {
  return {
    url: apiRoot(ctx) + "/ent/v2/tasks?task_ids=" + encodeURIComponent(ctx.taskId),
    method: "GET",
    headers: { Accept: "application/json", Authorization: "Bearer " + ctx.apiKey },
  };
}

function viduErrorMessage(body) {
  if (!body || typeof body !== "object" || Array.isArray(body)) return "";
  const error = body.error;
  if (error && typeof error === "object" && !Array.isArray(error)) {
    const message = trimmed(error.message || error.err_msg || error.detail);
    if (message) return message;
    const code = trimmed(error.code || error.err_code);
    if (code) return code;
  }
  if (body.code !== undefined && body.code !== null && String(body.code) !== "" && String(body.code) !== "0") {
    return trimmed(body.message || body.err_msg) || String(body.code);
  }
  return trimmed(body.err_msg || body.message || body.detail);
}

function viduFailureReason(body) {
  return viduErrorMessage(body) || trimmed(body.err_code) || "task failed";
}

function viduTaskRecords(body) {
  if (!body || typeof body !== "object" || Array.isArray(body)) return [];
  if (Array.isArray(body.tasks)) return body.tasks;
  if (body.data && typeof body.data === "object" && !Array.isArray(body.data)) return viduTaskRecords(body.data);
  if (body.task && typeof body.task === "object" && !Array.isArray(body.task)) return [body.task];
  if (body.id || body.task_id || body.state) return [body];
  return [];
}

function viduTaskRecord(body, taskId) {
  const records = viduTaskRecords(body);
  const wanted = trimmed(taskId);
  return records.find(function (record) {
    return record && (trimmed(record.id) === wanted || trimmed(record.task_id) === wanted);
  }) || (records.length === 1 ? records[0] : null);
}

function viduCreationEntries(record) {
  const creations = record && Array.isArray(record.creations) ? record.creations : [];
  return creations.filter(function (creation) {
    return creation && typeof creation === "object" && !Array.isArray(creation) && (trimmed(creation.url) || trimmed(creation.watermarked_url));
  });
}

export function parseTaskResult(ctx, body) {
  const error = viduErrorMessage(body);
  if (error) return { status: "FAILURE", progress: "100%", reason: error };
  const record = viduTaskRecord(body, ctx.taskId);
  if (!record) return { status: "UNKNOWN", reason: "task was not returned by the Vidu query endpoint" };
  const statuses = { created: "SUBMITTED", queueing: "SUBMITTED", processing: "IN_PROGRESS", success: "SUCCESS", failed: "FAILURE" };
  const rawState = trimmed(record.state).toLowerCase();
  const status = statuses[rawState];
  if (!status) return { status: "UNKNOWN", reason: "unknown task state: " + String(record.state || "") };
  const creations = viduCreationEntries(record);
  const url = creations.length ? trimmed(creations[0].url || creations[0].watermarked_url) : "";
  const result = {
    taskId: ctx.taskId,
    status: status,
    progress: status === "SUCCESS" || status === "FAILURE" ? "100%" : rawState === "processing" ? "50%" : "30%",
    reason: rawState === "failed" ? viduFailureReason(record) : "",
  };
  if (url) result.url = url;
  return result;
}

function artifactData(ctx) {
  const data = (ctx && ctx.data) || {};
  return viduTaskRecord(data, ctx && (ctx.taskId || ctx.task_id)) || data;
}

function artifactEntries(ctx) {
  return viduCreationEntries(artifactData(ctx)).map(function (creation) {
    return { url: trimmed(creation.url || creation.watermarked_url) };
  });
}

export function listArtifacts(task) {
  if (task.status !== "SUCCESS") return [];
  return artifactEntries(task).map(function (_entry, index) {
    return { key: index === 0 ? "video" : "video-" + (index + 1), type: "video", mimeType: "video/mp4" };
  });
}

export function buildContentRequest(ctx) {
  const entries = artifactEntries(ctx);
  let index = 0;
  if (ctx.artifactKey !== "video") {
    const match = /^video-(\d+)$/.exec(String(ctx.artifactKey || ""));
    if (!match) throw new Error("artifact_not_found");
    index = Number(match[1]) - 1;
  }
  const url = entries[index] ? entries[index].url : "";
  if (!url) throw new Error("artifact_not_found");
  return { url: url, method: ctx.clientRequest.method, credentialless: true };
}

export function extractUsageOnComplete(_task, _taskResult, _body) {
  return null;
}

function nativeJSONBody(ctx) {
  if (!ctx.body || ctx.body.kind !== "json") throw new Error("JSON body required");
  const body = ctx.body.value;
  if (!body || typeof body !== "object" || Array.isArray(body)) throw new Error("request body must be an object");
  return body;
}

function nativeModel(body, fallback) {
  const model = trimmed(body && body.model) || fallback || "";
  if (!VIDU_MODELS.includes(model)) throw new Error("unsupported Vidu model: " + model);
  return model;
}

function nativeArray(body, key, required) {
  if (!Array.isArray(body[key])) {
    if (!required && body[key] === undefined) return [];
    throw new Error(key + " must be an array");
  }
  return body[key];
}

function nativePrompt(body, required) {
  if (body.prompt === undefined || body.prompt === null) {
    if (required) throw new Error("prompt must be a non-empty string");
    return;
  }
  if (typeof body.prompt !== "string" || (required && !trimmed(body.prompt))) throw new Error("prompt must be a non-empty string");
}

function nativeVideoIntent(body, model, action) {
  const duration = body.duration === undefined ? undefined : Number(body.duration);
  validateViduCombo(model, duration, outboundResolution(body, model), action !== "text_to_video");
  return { kind: "submit", model: model, action: action, requestBody: Object.assign({}, body, { model: model }) };
}

function createTextVideoTask(ctx) {
  const body = nativeJSONBody(ctx);
  const model = nativeModel(body);
  nativePrompt(body, true);
  return nativeVideoIntent(body, model, "text_to_video");
}

function createImageVideoTask(ctx) {
  const body = nativeJSONBody(ctx);
  const model = nativeModel(body);
  const images = nativeArray(body, "images", true);
  if (images.length !== 1) throw new Error("images must contain exactly one image");
  nativePrompt(body, false);
  return nativeVideoIntent(body, model, "image_to_video");
}

function createReferenceVideoTask(ctx) {
  const body = nativeJSONBody(ctx);
  const model = nativeModel(body);
  const subjects = nativeArray(body, "subjects", false);
  const images = nativeArray(body, "images", false);
  const videos = nativeArray(body, "videos", false);
  if (subjects.length === 0 && images.length === 0 && videos.length === 0) throw new Error("subjects, images, or videos is required");
  nativePrompt(body, true);
  return nativeVideoIntent(body, model, "reference_to_video");
}

function createFirstTailVideoTask(ctx) {
  const body = nativeJSONBody(ctx);
  const model = nativeModel(body);
  const images = nativeArray(body, "images", true);
  if (images.length !== 2) throw new Error("images must contain exactly two images");
  nativePrompt(body, false);
  return nativeVideoIntent(body, model, "first_tail_to_video");
}

function createMultiframeTask(ctx) {
  const body = nativeJSONBody(ctx);
  const model = nativeModel(body);
  if (model !== "viduq2-turbo" && model !== "viduq2-pro") throw new Error("multiframe supports viduq2-turbo and viduq2-pro");
  if (typeof body.start_image !== "string" || !trimmed(body.start_image)) throw new Error("start_image is required");
  const settings = nativeArray(body, "image_settings", true);
  if (settings.length < 2 || settings.length > 9) throw new Error("image_settings must contain between 2 and 9 keyframes");
  for (const item of settings) {
    if (!item || typeof item !== "object" || Array.isArray(item) || typeof item.key_image !== "string" || !trimmed(item.key_image))
      throw new Error("each image_settings item requires key_image");
    if (item.duration !== undefined && (!Number.isInteger(Number(item.duration)) || Number(item.duration) < 2 || Number(item.duration) > 7))
      throw new Error("image_settings duration must be between 2 and 7 seconds");
  }
  return { kind: "submit", model: model, action: "multiframe", requestBody: Object.assign({}, body, { model: model }) };
}

function createTemplateTask(ctx) {
  const body = nativeJSONBody(ctx);
  if (typeof body.template !== "string" || !trimmed(body.template)) throw new Error("template is required");
  const images = nativeArray(body, "images", true);
  if (images.length === 0) throw new Error("images must not be empty");
  const model = nativeModel(body, VIDU_DEFAULT_NATIVE_MODEL);
  return { kind: "submit", model: model, action: "template", requestBody: Object.assign({}, body, { model: model }) };
}

function createTemplateStoryTask(ctx) {
  const body = nativeJSONBody(ctx);
  if (typeof body.story !== "string" || !trimmed(body.story)) throw new Error("story is required");
  const images = nativeArray(body, "images", true);
  if (images.length === 0) throw new Error("images must not be empty");
  const model = nativeModel(body, VIDU_DEFAULT_NATIVE_MODEL);
  return { kind: "submit", model: model, action: "template_story", requestBody: Object.assign({}, body, { model: model }) };
}

function queryListValues(query, names) {
  for (const name of names) {
    const values = (query && query[name]) || [];
    if (values.length > 1) return values.reduce(function (all, value) {
      return all.concat(String(value).split(","));
    }, []).map(trimmed).filter(Boolean);
    if (values.length === 1 && trimmed(values[0])) return String(values[0]).split(",").map(trimmed).filter(Boolean);
  }
  return [];
}

function querySingleValue(query, names, fallback) {
  for (const name of names) {
    const values = (query && query[name]) || [];
    if (values.length > 1) throw new Error(name + " must be provided once");
    if (values.length === 1 && trimmed(values[0])) return trimmed(values[0]);
  }
  return fallback;
}

function nativeViduStatuses(states) {
  const stateMap = { created: "queued", queueing: "queued", processing: "running", success: "succeeded", failed: "failed" };
  const statuses = [];
  for (const state of states) {
    const mapped = stateMap[state];
    if (!mapped) throw new Error("states contains an invalid value");
    if (!statuses.includes(mapped)) statuses.push(mapped);
  }
  return statuses;
}

function nativeViduModelVersions(versions) {
  const models = [];
  for (const version of versions) {
    let matches;
    if (version === "q1") matches = VIDU_MODELS.filter(function (model) { return model.indexOf("viduq1") === 0; });
    else if (version === "q2") matches = VIDU_MODELS.filter(function (model) { return model.indexOf("viduq2") === 0; });
    else if (version === "q3") matches = VIDU_MODELS.filter(function (model) { return model.indexOf("viduq3") === 0; });
    else if (version === "2.0") matches = ["vidu2.0"];
    else throw new Error("model_versions contains an invalid value");
    for (const model of matches) {
      if (!models.includes(model)) models.push(model);
    }
  }
  return models;
}

function nativeViduQueryTime(value, name) {
  const raw = trimmed(value);
  if (!raw) return 0;
  const numeric = Number(raw);
  if (Number.isFinite(numeric) && numeric >= 0) return Math.floor(numeric);
  const parsed = Date.parse(raw);
  if (!Number.isFinite(parsed) || parsed < 0) throw new Error(name + " is invalid");
  return Math.floor(parsed / 1000);
}

function nativeViduTask(task) {
  const record = viduTaskRecord(task && task.data, task && task.task_id);
  const result = record && typeof record === "object" && !Array.isArray(record) ? Object.assign({}, record) : {};
  const publicTaskID = trimmed(task && task.task_id);
  delete result.task_id;
  if (publicTaskID) result.id = publicTaskID;
  if (!result.state) {
    const states = { NOT_START: "created", SUBMITTED: "created", QUEUED: "queueing", IN_PROGRESS: "processing", SUCCESS: "success", FAILURE: "failed" };
    result.state = states[task && task.status] || "created";
  }
  if (!result.created_at && task && task.created_at) result.created_at = task.created_at;
  if (result.state === "failed" && !result.err_msg && trimmed(task && task.fail_reason)) result.err_msg = trimmed(task.fail_reason);
  return result;
}

export const native = {
  createTextVideoTask: createTextVideoTask,
  createImageVideoTask: createImageVideoTask,
  createReferenceVideoTask: createReferenceVideoTask,
  createFirstTailVideoTask: createFirstTailVideoTask,
  createMultiframeTask: createMultiframeTask,
  createTemplateTask: createTemplateTask,
  createTemplateStoryTask: createTemplateStoryTask,
  decodeViduTaskList: function (ctx) {
    if (!ctx.body || ctx.body.kind !== "none") throw new Error("request body is not allowed");
    const query = ctx.query || {};
    const model = querySingleValue(query, ["filter.model", "model"], "");
    if (model) nativeModel({ model: model });
    const taskIds = queryListValues(query, ["task_ids", "filter.task_ids"]);
    if (taskIds.length > 100) throw new Error("task_ids accepts at most 100 task IDs");
    const states = queryListValues(query, ["states", "filter.states"]);
    const modelVersions = queryListValues(query, ["model_versions"]);
    const modelVersionModels = nativeViduModelVersions(modelVersions);
    if (model && modelVersionModels.length && !modelVersionModels.includes(model)) throw new Error("model and model_versions do not match");
    const templates = queryListValues(query, ["templates"]);
    const resolutions = queryListValues(query, ["resolutions"]);
    if (templates.length > 100 || resolutions.length > 100) throw new Error("templates and resolutions accept at most 100 values");
    const dataFilters = {};
    if (templates.length) dataFilters.template = templates;
    if (resolutions.length) dataFilters.resolution = resolutions;
    const page = Number(querySingleValue(query, ["pager.page", "page_num"], "1"));
    const pageSize = Number(querySingleValue(query, ["pager.pagesz", "page_size"], "20"));
    if (!Number.isInteger(page) || page < 1 || page > 100000 || !Number.isInteger(pageSize) || pageSize < 1 || pageSize > 100)
      throw new Error("pager.page must be 1-100000 and pager.pagesz must be 1-100");
    const pageToken = querySingleValue(query, ["pager.page_token"], "");
    if (pageToken) {
      const tokenPage = Number(pageToken);
      if (!Number.isInteger(tokenPage) || tokenPage < 1 || tokenPage > 100000) throw new Error("pager.page_token is invalid");
    }
    const createdAfter = nativeViduQueryTime(querySingleValue(query, ["created_at.from"], ""), "created_at.from");
    const createdBefore = nativeViduQueryTime(querySingleValue(query, ["created_at.to"], ""), "created_at.to");
    if (createdAfter > 0 && createdBefore > 0 && createdAfter >= createdBefore) throw new Error("created_at.from must be before created_at.to");
    const listOptions = { pageNum: pageToken ? Number(pageToken) : page, pageSize: pageSize };
    if (createdAfter > 0) listOptions.createdAfter = createdAfter;
    if (createdBefore > 0) listOptions.createdBefore = createdBefore;
    const intent = {
      kind: "query",
      model: model,
      taskIds: taskIds,
      actions: VIDU_ACTIONS,
      statuses: nativeViduStatuses(states),
      listOptions: listOptions,
    };
    if (modelVersionModels.length && !model) intent.models = modelVersionModels;
    if (Object.keys(dataFilters).length) intent.dataFilters = dataFilters;
    return intent;
  },
  viduTaskCreated: function (ctx, task) {
    const result = task && task.data && typeof task.data === "object" && !Array.isArray(task.data) ? Object.assign({}, task.data) : {};
    result.task_id = task && task.task_id;
    if (!result.state) {
      const states = { NOT_START: "created", SUBMITTED: "created", QUEUED: "queueing", IN_PROGRESS: "processing", SUCCESS: "success", FAILURE: "failed" };
      result.state = states[task && task.status] || "created";
    }
    if (result.state === "failed" && !result.err_msg && trimmed(task && task.fail_reason)) result.err_msg = trimmed(task.fail_reason);
    return result;
  },
  viduTaskList: function (ctx, result) {
    const items = Array.isArray(result && result.items) ? result.items.map(nativeViduTask) : [];
    const page = Number(result && result.page_num) || 1;
    const pageSize = Number(result && result.page_size) || 20;
    const total = Number(result && result.total) || 0;
    return { next_page_token: page * pageSize < total ? String(page + 1) : "", tasks: items };
  },
  error: function (ctx, error) {
    return { code: error.code, message: error.message };
  },
};

export const protocols = {
  openai_responses: {
    decodeRequest: function (ctx) {
      if (!ctx.body || ctx.body.kind !== "json") throw new Error("JSON body required");
      const req = ctx.body.value;
      if (!req || typeof req !== "object" || Array.isArray(req)) throw new Error("request body must be an object");
      const model = trimmed(req.model);
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
      const requestBody = { model: model, prompt: prompt };
      if (images.length) requestBody.images = images;
      if (Object.prototype.hasOwnProperty.call(req, "seconds")) requestBody.duration = req.seconds;
      else if (Object.prototype.hasOwnProperty.call(req, "duration")) requestBody.duration = req.duration;
      if (Object.prototype.hasOwnProperty.call(req, "size")) requestBody.size = req.size;
      if (Object.prototype.hasOwnProperty.call(req, "metadata")) requestBody.metadata = req.metadata;
      const duration = requestBody.duration === undefined ? undefined : Number(requestBody.duration);
      validateViduCombo(model, duration, outboundResolution(requestBody, model), images.length > 0);
      return { kind: "submit", model: model, action: actionFor(requestBody), requestBody: requestBody };
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
        metadata: { vendor: "vidu" },
      };
    },
  },
};

const legacyRenderers = {
  openai_video: function (task) {
    const statusMap = { NOT_START: "queued", SUBMITTED: "queued", QUEUED: "queued", IN_PROGRESS: "in_progress", SUCCESS: "completed", FAILURE: "failed" };
    const output = {
      id: task.task_id,
      object: "video",
      model: task.properties && task.properties.origin_model_name ? task.properties.origin_model_name : "",
      status: statusMap[task.status] || "unknown",
      progress: Number(String(task.progress || "0").replace("%", "")),
      created_at: task.created_at,
    };
    if (task.updated_at) output.completed_at = task.updated_at;
    if (task.data && task.data.state === "failed") {
      const message = viduFailureReason(task.data);
      if (message) output.error = { message: message, code: trimmed(task.data.err_code) || "task_failed" };
    }
    return output;
  },
};

protocols.openai_video = {
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
          throw new Error("metadata must be a JSON object string");
        }
        if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error("metadata must be a JSON object string");
        req.metadata = parsed;
      }
      if (req.seconds !== undefined) req.seconds = Number(req.seconds);
      else if (req.duration !== undefined) req.seconds = Number(req.duration);
    }
    const model = ctx.upstreamModel || ctx.model || req.model;
    const seconds = req.seconds === undefined ? req.duration : req.seconds;
    if (seconds !== undefined) req.duration = Number(seconds);
    else req.duration = defaultDuration(model);
    if (hasInputReferenceFile) {
      req.images = [{ __fileRef: "request_file:input_reference", encoding: "dataUrl", maxBytes: 15728640 }];
    } else {
      const image = trimmed(req.input_reference || req.image);
      if (image && (!Array.isArray(req.images) || req.images.length === 0)) req.images = [image];
    }
    req.resolution = outboundResolution(req, model);
    const hasImages = hasViduImages(req, hasInputReferenceFile);
    validateViduCombo(model, req.duration, req.resolution, hasImages);
    return {
      kind: "submit",
      model: ctx.model,
      action: actionFor(req),
      requestBody: Object.assign({}, req, { model: ctx.model }),
    };
  },
  render: function (ctx, task) {
    return legacyRenderers.openai_video(task);
  },
};
