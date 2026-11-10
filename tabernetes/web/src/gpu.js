// The WebGPU side of the kubelets in the browser (k8s/tabernetes/tabnode_gpu.go),
// as globalThis.tabernetesGPU. The tab node and every node in a Web Worker
// (addnode --gpu) probe the adapter of the browser with it and publish it as a
// DRA device of the driver webgpu.tabernetes.io. WebGPU works on the page and in
// dedicated workers (Chrome, Edge; Firefox and Safari behind flags or newer
// versions), so a node in a worker sees the same adapter as the tab.
//
// The probe follows the adapter probe of upstream's WebLLM tab node
// (jkroepke/blog, tabernetes/web/src/gpu.js) and adds the features and limits
// that a WebAssembly workload may select on.

// The limits that become device attributes. A request selects on them with
// CEL, for example device.attributes["webgpu.tabernetes.io"].maxBufferSize.
const LIMITS = [
  "maxBufferSize",
  "maxStorageBufferBindingSize",
  "maxComputeWorkgroupStorageSize",
  "maxComputeInvocationsPerWorkgroup",
  "maxComputeWorkgroupsPerDimension",
];

export function installGPU() {
  globalThis.tabernetesGPU = {
    // probe resolves to a JSON string that describes the adapter, or to ""
    // without WebGPU or without an adapter. JSON keeps the Go side free of
    // property lookups on JS objects.
    async probe() {
      const gpu = globalThis.navigator?.gpu;
      if (!gpu) return "";
      const a = await gpu.requestAdapter({ powerPreference: "high-performance" }).catch(() => null);
      if (!a) return "";
      const i = a.info || {};
      const limits = {};
      for (const k of LIMITS) if (typeof a.limits[k] === "number") limits[k] = a.limits[k];
      return JSON.stringify({
        vendor: i.vendor || "",
        architecture: i.architecture || "",
        device: i.device || "",
        description: i.description || "",
        // A software adapter (SwiftShader, WARP). Chrome hands it out only
        // with --enable-unsafe-webgpu --use-webgpu-adapter=swiftshader or
        // when no hardware adapter is there.
        fallback: Boolean(i.isFallbackAdapter ?? a.isFallbackAdapter),
        features: [...a.features].sort(),
        limits,
        jspi: typeof WebAssembly.Suspending === "function" && typeof WebAssembly.promising === "function",
      });
    },
  };
}
