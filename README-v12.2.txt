OpenLinkHub iCUE profile playback v12 - capture-driven effects
==============================================================

Baseline: v11 audited consolidated build.

Added from the 2026-10-14 iCUE laptop capture/audit:

1. ColorWaveLighting renderer
   - Parses the authored two colours.
   - Uses captured speed semantics: 1=slow, 2=normal, 3=fast.
   - Uses captured direction semantics: 2=Left, 1=Right.
   - Uses device topology coordinates where known.

2. WaveLighting renderer
   - Uses captured velocity (lights/sec), tailLength (lights), and isDoublesided.
   - Renders the authored transition colour/alpha curve along the moving tail.
   - Preserves angle in parsing but does not invent a 2-D angle interpretation for
     the current one-dimensional LINK topology.

3. LightingLink resolution
   - Parses the profile-level LightingLinkProperty.
   - Expands device LightingLink wrappers to the shared authored lighting layer.
   - The captured CCL-Sci-fi Dark profile resolves Lighting Link 1 to its authored
     StaticLighting layer rather than treating LightingLink as a separate effect.

4. Compatibility registry
   - ColorWaveLighting, WaveLighting and LightingLink now have playback handlers.

Validation performed here:
   - gofmt on all changed Go files.
   - src/icueprofile compiled/tests passed in an isolated Go 1.23 module (the full
     OpenLinkHub tree requires Go >=1.25, which is not installed in this environment).
   - CCL-Sci-fi Dark parsed with one LightingLinkProperty layer resolved to
     StaticLighting; QX RenderFan returned a 102-byte frame with no unsupported LINK
     effects for the tested captured profile.
   - The captured custom two-sided Wave profile parsed/rendered without panic.

Important fidelity boundary:
   The parameter meanings above are capture-derived. The exact proprietary Corsair
   temporal interpolation equations are not present in the profile files. ColorWave
   and Wave therefore use continuous renderer equations driven by the captured
   parameters; they are not claimed to be byte/frame-identical to iCUE. No unknown
   direction/angle values are invented.

v12.2 clean TITAN ConcreteLeds update
-------------------------------------
- Adds the capture-verified iCUE LINK TITAN 280 ConcreteLeds range (Led_Cmd_Led756..775).
- Keeps the existing SDK-derived 20-LED TITAN spatial topology unchanged.
- Enables ConcreteLeds selections on TITAN for imported profiles such as CCL-Sci-fi Dark and Halo-MasterChief.
- Does not include the temporary v12.1 Sci-Fi layer-trace diagnostics.
