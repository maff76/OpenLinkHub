OpenLinkHub iCUE profile playback v12.4 - expanded capture-driven effects
=======================================================================

Baseline: clean v12.3 measured TITAN mapping build.
Evidence: 2026-10-15 laptop capture capture-20261015-182518.zip.

New playback handlers
---------------------
1. ColorPulseLighting
   - Uses the captured two-colour + speed structure.
   - Smooth A->B->A pulse across the authored selection.

2. ColorShiftLighting
   - Uses the captured two-colour + speed structure.
   - Whole-selection colour transition; no invented spatial movement.

3. RainbowWaveLighting
   - Uses captured speed and direction.
   - Uses known device topology coordinates where available.

4. WaterColorLighting
   - Parses authored endpoint colours, isRandom and background.
   - Renders broad deterministic moving colour pools; random mode adds stable
     pseudo-random variation rather than frame-to-frame noise.

5. RainbowLighting
   - Whole-selection spectrum cycle using the captured speed control.

6. RainbowChannelLighting
   - Spatial spectrum across a channel using captured speed and direction.

7. WatercolorSpectrumLighting
   - Spatial low-saturation spectrum pools using captured speed and direction.

Parser additions
----------------
- Preserves isRandom and background for WaterColorLighting.
- Preserves autoDuration and waveSpread for RippleLighting diagnostics.
- Existing velocity, tailLength, transitions and colours expose the rest of the
  captured Ripple payload needed for future reactive playback work.

Ripple boundary
---------------
RippleLighting is intentionally NOT marked supported in this build. The Sci-Fi
capture proves that its two layers are reactive OnPressRipple effects using
startOnKeyPress, PlayFromKeyCenter and one-shot playback. Treating those as a
continuous animation would make the compatibility number look better while
being behaviourally wrong. The captured Ripple parameters are retained for the
next input/reactive-renderer stage.

Capture validation
------------------
Using the current parser/compatibility registry against the 2026-10-15 capture:
- CCL-Sci-fi Dark: 192/194 layers supported; only 2x RippleLighting remain.
- Halo-MasterChief: 101/101 supported.
- Halo-Infinite: 180/180 supported.
- Profile 2: 11/11 supported.
- Profile 4: 8/8 supported.
- Profile 5: 8/8 supported.
- Profile 7: 8/8 supported.
- Default Profile: 46/57 supported.
- rain test: 19/25 supported.

Validation performed in this environment
----------------------------------------
- gofmt on changed Go files.
- src/icueprofile and cmd/icueprofile-inspect compile under an isolated Go 1.23
  module and `go test ./...` passes.
- The full repository declares Go 1.25. This offline environment has Go 1.23.2
  and cannot download the Go 1.25 toolchain, so a full-tree build could not be
  performed here.

Fidelity boundary
-----------------
The serialized fields and effect classifications are capture-derived. Corsair's
proprietary temporal interpolation equations are not contained in the profile
files. The new continuous renderers therefore follow the captured controls and
selection/topology semantics but are not claimed to be frame-identical to iCUE.
No profile-specific colour or LED hacks are added.
