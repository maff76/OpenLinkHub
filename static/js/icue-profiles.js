$(function () {
    let currentKey='', currentData=null, currentTarget=null, currentLayer=null, applying=false, probeLocal=0, liveDirty=false;
    let detectedDevices=[], mappingObservations=[];
    // Measured TITAN 280 local-output -> iCUE UI zone map. Logical IDs are
    // 756..775 in local output order, matching the verified Go topology.
    const titan280Zones=[6,7,8,9,10,11,12,13,14,15,16,1,2,3,4,5,17,18,19,20];
    const commonEffects=['StaticLighting','RainLighting','WaveLighting','ColorWaveLighting','RippleLighting','SpiralLighting','SpiralRainbowLighting','RainbowWaveLighting','WatercolorLighting','VisorLighting','MarqueeLighting','SequentialLighting','ColorShiftLighting','ColorPulseLighting','GradientLighting'];
    const field=(keys,label,type='number')=>({keys:Array.isArray(keys)?keys:[keys],label,type});
    const colours=field(['transitions','colors','color'],'Colours','colors');
    const speed=field(['speed','velocity'],'Speed / velocity');
    const direction=field(['direction','angle'],'Direction / angle');
    const duration=field(['duration','autoDuration'],'Duration');
    const width=field(['width','waveSpread','spread','size'],'Width / spread');
    const tail=field(['tailLength','tail','falloff'],'Tail / falloff');
    const phase=field(['phase','offset'],'Phase / offset');
    const schemas={
        staticlighting:[colours,field('brightness','Brightness'),field('opacity','Opacity')],
        rainlighting:[colours,speed,direction,duration,tail,field('brightness','Brightness'),field('opacity','Opacity')],
        wavelighting:[colours,speed,direction,width,field(['wavelength','period','frequency'],'Wavelength / period'),tail,phase,field('isDoublesided','Double-sided','bool')],
        colorwavelighting:[colours,speed,direction,width,field(['wavelength','period','frequency'],'Wavelength / period'),tail,phase,field('isDoublesided','Double-sided','bool')],
        ripplelighting:[colours,speed,duration,width,tail,direction,field('autoDuration','Auto duration','bool')],
        spirallighting:[colours,speed,direction,width,tail,phase,field('clockwise','Clockwise','bool')],
        spiralrainbowlighting:[speed,direction,width,phase,field('clockwise','Clockwise','bool')],
        rainbowwavelighting:[speed,direction,width,phase],
        watercolorlighting:[colours,speed,duration,width,field('brightness','Brightness'),field('opacity','Opacity')],
        visorlighting:[colours,speed,direction,width,tail,duration],
        marqueelighting:[colours,speed,direction,width,field(['spacing','period'],'Spacing / period')],
        sequentiallighting:[colours,speed,direction,duration,field(['step','steps'],'Step / steps')],
        colorshiftlighting:[colours,speed,duration,phase],
        colorpulselighting:[colours,speed,duration,phase],
        gradientlighting:[colours,direction,field('brightness','Brightness'),field('opacity','Opacity')]
    };
    const boolKeys=new Set(['clockwise','random','isRandom','isDoublesided','reverse','autoDuration']);
    function status(t){$('#icueLiveStatus').text(t);} function layerLabel(l,i){return (l.name&&l.name.trim())||l.effectName||l.effect||('Layer '+(i+1));}
    // The Go parser has already extracted the authored colour leaves from the
    // .cueprofile.  For display, consume those leaves directly instead of
    // routing them back through the renderer/canonical playback model.
    function authoredColorFromLeaf(raw){
        const m=String(raw||'').trim().match(/^color\s*=\s*#([0-9a-fA-F]{6}|[0-9a-fA-F]{8})$/i);
        if(!m)return null;
        const hex=m[1].length===8?m[1].slice(2):m[1];
        return '#'+hex.toLowerCase();
    }
    function authoredColors(){
        const out=[];
        const add=raw=>{const c=authoredColorFromLeaf(raw);if(c&&!out.includes(c))out.push(c);};
        ((currentLayer&&currentLayer.rawColorValues)||[]).forEach(add);
        if(!out.length){
            const a=authoredParameters();
            ['color','colors','transitions'].forEach(k=>{const p=a[k];if(p&&Array.isArray(p.rawValues))p.rawValues.forEach(add);});
        }
        return out;
    }
    function editorSourceTrace(){const colors=authoredColors(),raw=(currentLayer&&currentLayer.rawColorValues)||[],trace={backendVersion:(currentData&&((currentData.editor||{}).version||currentData.editorBackendVersion))||null,layerIndex:currentTarget?currentTarget.layerIndex:null,layerName:currentLayer?layerLabel(currentLayer,currentTarget?currentTarget.layerIndex:0):'',authored:{rawColorValues:raw,displayColors:colors,parameters:authoredParameters()}};let d=$('#icueEditorBoundaryTrace');if(!d.length){d=$('<details id="icueEditorBoundaryTrace" class="mt-3"><summary>Authored source values</summary><pre class="mt-2 mb-0" style="white-space:pre-wrap;max-height:320px;overflow:auto"></pre></details>');$('#icueParameters').parent().append(d);}d.find('pre').text(JSON.stringify(trace,null,2));}
    function hexToRGB(h){return 'rgb('+[1,3,5].map(i=>(parseInt(h.slice(i,i+2),16)/255).toFixed(6).replace(/0+$/,'').replace(/\.$/,'')).join(' ')+')';}
    function authoredParameters(){return (currentLayer&&currentLayer.authoredParameters)||{};} function authoredValue(key){const a=authoredParameters()[key];return a&&a.value!==undefined?a.value:((currentLayer&&currentLayer.parameters)||{})[key];} function profileColor(){return authoredColors()[0]||'#2563eb';}
    let timingCalibration={slow:1.5,fast:0.5},timingApplyTimer=null;
    let rainMethod='normalized';
    function syncRainMethodUI(){
        $('.icue-rain-method').each(function(){const on=$(this).data('method')===rainMethod;$(this).toggleClass('active',on).toggleClass('secondary',!on);});
        $('#icueRainMethodStatus').text((rainMethod==='native'?'Native Rain':'Normalized Rain')+' · runtime only.');
    }
    function loadRainMethod(){return $.getJSON('/api/icue/rain-method').then(r=>{if(r&&r.rain&&r.rain.method)rainMethod=r.rain.method;syncRainMethodUI();});}
    function applyRainMethod(method){if(method!=='normalized'&&method!=='native')return;$('#icueRainMethodStatus').text('Switching Rain implementation…');$.ajax({url:'/api/icue/rain-method',type:'POST',contentType:'application/json',data:JSON.stringify({method}),success:r=>{rainMethod=(r&&r.rain&&r.rain.method)||method;syncRainMethodUI();},error:x=>{$('#icueRainMethodStatus').text('Rain switch failed.');toast.warning(x.responseText||'Unable to switch Rain implementation');}});}
    function timingScaleFor(speed){return speed===1?timingCalibration.slow:(speed===3?timingCalibration.fast:1.0);}
    function syncTimingCalibrationUI(){const slow=Number(timingCalibration.slow),fast=Number(timingCalibration.fast);$('#icueSlowScale,#icueSlowScaleRange').val(slow);$('#icueFastScale,#icueFastScaleRange').val(fast);updateEffectClock();}
    function loadTimingCalibration(){return $.getJSON('/api/icue/timing-calibration').then(r=>{if(r&&r.timing)timingCalibration={slow:Number(r.timing.slow)||1.5,fast:Number(r.timing.fast)||0.5};syncTimingCalibrationUI();});}
    function applyTimingCalibration(slow,fast){slow=Math.max(1,Math.min(3,Number(slow)));fast=Math.max(0.2,Math.min(1,Number(fast)));if(!Number.isFinite(slow)||!Number.isFinite(fast))return;timingCalibration={slow,fast};syncTimingCalibrationUI();$('#icueTimingCalibrationStatus').text('Applying globally…');clearTimeout(timingApplyTimer);timingApplyTimer=setTimeout(()=>{$.ajax({url:'/api/icue/timing-calibration',type:'POST',contentType:'application/json',data:JSON.stringify(timingCalibration),success:r=>{if(r&&r.timing)timingCalibration={slow:Number(r.timing.slow),fast:Number(r.timing.fast)};syncTimingCalibrationUI();$('#icueTimingCalibrationStatus').text('Live globally · Normal fixed at 1.0× · runtime only.');},error:x=>{$('#icueTimingCalibrationStatus').text('Calibration update failed.');toast.warning(x.responseText||'Unable to update timing calibration');}});},120);}

    function updateEffectClock(){
        if(!currentLayer)return;
        const rawDuration=Number(authoredValue('duration')??1000);
        const duration=Number.isFinite(rawDuration)&&rawDuration>0?rawDuration:1000;
        const rawSpeed=Number(authoredValue('speed')??2);
        const speed=rawSpeed<1?1:(rawSpeed>3?3:rawSpeed);
        const scale=timingScaleFor(speed);
        const effective=duration*scale;
        const label=speed===1?'Slow':(speed===3?'Fast':'Normal');
        $('#icueClockLayer').text(layerLabel(currentLayer,currentTarget?currentTarget.layerIndex:0));
        $('#icueClockDuration').text(Math.round(duration)+' ms');
        $('#icueClockSpeed').text(label+' ('+speed+')');
        $('#icueClockScale').text(scale.toFixed(1)+'×');
        $('#icueClockWindow').text(Math.round(effective)+' ms');
    }
    function refreshDevices(){return $.getJSON('/api/icue/devices').then(r=>{detectedDevices=r.devices||[];const qx=detectedDevices.filter(d=>d.kind==='qx');qx.forEach((d,i)=>d.qxOrdinal=i);const memory=detectedDevices.filter(d=>d.kind==='memory');memory.forEach((d,i)=>d.memoryOrdinal=i);const sel=$('#icueDeviceCanvas').empty();if(qx.length>1)sel.append($('<option>').val('group:qx').text('All QX Fans · '+qx.length));if(memory.length>1)sel.append($('<option>').val('group:memory').text('All Memory · '+memory.length));detectedDevices.forEach((d,i)=>sel.append($('<option>').val(i).text(d.kind==='memory'&&d.locator?d.name+' · '+d.locator:d.name)));if(!detectedDevices.length)sel.append($('<option>').val('').text('No RGB devices detected'));const groups={};detectedDevices.forEach(d=>groups[d.kind]=(groups[d.kind]||0)+1);$('#icueDetectedSummary').text('Detected: '+Object.entries(groups).map(x=>x[1]+' '+x[0].toUpperCase()).join(' · '));});}
    function refreshProfiles(selectKey){$.getJSON('/api/icue/profiles',r=>{ $('#icueProfileDirectory').text(r.directory?'Stored in: '+r.directory:'');const list=$('#icueProfileList').empty();(r.profiles||[]).forEach(p=>{const row=$('<div class="settings-row icue-profile-row"></div>').attr('data-key',p.key);row.append($('<span class="settings-label text-ellipsis"></span>').text(p.name||p.fileName),$('<small></small>').text(p.layers+' layers')).on('click',()=>loadProfile(p.key));list.append(row);});if(!(r.profiles||[]).length)list.append('<div class="settings-row"><span class="settings-label">No iCUE profiles imported</span></div>');if(selectKey)loadProfile(selectKey);});}
    function workspaceLayers(data){const p=data.profile||{},out=[];let di=(p.devices||[]).findIndex(d=>Number(d.usbVid)===6940&&Number(d.usbPid)===3135&&(d.channels||[]).length);if(di<0)return out;const c=p.devices[di].channels[0];(c.layers||[]).forEach((l,li)=>{if(String(l.effect||'').toLowerCase()==='lightinglink'&&(data.lightingLinkLayers||[]).length){data.lightingLinkLayers.forEach((linked,lli)=>{const v=Object.assign({},linked,{enabled:!!l.enabled&&!!linked.enabled,selectionMode:l.selectionMode,leds:Array.isArray(l.leds)?l.leds.slice():[]});out.push({layer:v,deviceIndex:di,channelIndex:0,layerIndex:lli,lightingLink:true,selectionSource:l,selectionLayerIndex:li});});}else out.push({layer:l,deviceIndex:di,channelIndex:0,layerIndex:li,lightingLink:false});});return out;}
    function resetRuntimeProfile(key,quiet){if(!key)return $.Deferred().resolve().promise();return $.ajax({url:'/api/icue/profile?key='+encodeURIComponent(key),type:'DELETE'}).then(()=>{if(!quiet)toast.success('Live preview reset to imported profile');});}
    let profileLoadSerial=0;
    function loadProfileData(key){
        const serial=++profileLoadSerial;
        status('Loading profile…');
        return $.getJSON('/api/icue/profile?key='+encodeURIComponent(key)).done(data=>{
            if(serial!==profileLoadSerial)return;
            currentKey=key;currentData=data;currentTarget=null;currentLayer=null;
            const backend=((data.editor||{}).version||data.editorBackendVersion||'UNKNOWN');console.info('[ICUE-EDITOR-BACKEND]',backend,'source=authored-cueprofile');
            $('.icue-profile-row').removeClass('active').filter(function(){return $(this).attr('data-key')===key;}).addClass('active');
            $('#icueEmpty').hide();$('#icueWorkspace').show();const p=data.profile||{};$('#icueProfileTitle').text(p.name||data.fileName||key);$('#icueMetaFile').text(data.fileName||'—');$('#icueMetaDevices').text((p.summary||{}).deviceEntries??(p.devices||[]).length);$('#icueMetaChannels').text((p.summary||{}).channels??'—');const layers=workspaceLayers(data);$('#icueMetaLayers').text(layers.length);const list=$('#icueLayerList').empty();layers.forEach((x,i)=>{const row=$('<div class="settings-row icue-layer-row"></div>').attr('data-i',i);row.append($('<span class="settings-label text-ellipsis"></span>').text((i+1)+' · '+layerLabel(x.layer,i)),$('<span class="icue-layer-effect"></span>').text(x.layer.effect||'unknown')).on('click',()=>selectLayer(x,i));list.append(row);});if(!layers.length)list.append('<div class="settings-row"><span class="settings-label">No iCUE LINK lighting layers found</span></div>');$('#icueSaveName').val((p.name||'Profile')+'-EDIT-001');if(layers.length)selectLayer(layers[0],0);else status('Profile loaded · no editable iCUE LINK layers found.');
        }).fail(x=>{if(serial===profileLoadSerial){toast.warning(x.responseText||'Unable to load iCUE profile');status('Profile load failed.');}});
    }
    function activateHardwareProfile(key){
        const targets=new Map();
        detectedDevices.forEach(d=>{
            const raw=String(d.key||'');
            const cut=raw.lastIndexOf(':');
            if(cut<=0)return;
            const deviceId=raw.slice(0,cut),channelId=Number(raw.slice(cut+1));
            if(!deviceId||!Number.isInteger(channelId))return;
            targets.set(deviceId+':'+channelId,{deviceId,channelId,profile:key});
        });
        if(!targets.size)return $.Deferred().reject({responseText:'No detected RGB hardware targets are available for this profile.'}).promise();
        const requests=[...targets.values()].map(body=>$.ajax({url:'/api/color',type:'POST',contentType:'application/json',data:JSON.stringify(body)}).then(r=>{
            if(!r||Number(r.status)!==1)return $.Deferred().reject({responseText:(r&&r.message)||('Hardware rejected '+key)}).promise();
            return r;
        }));
        return $.when.apply($,requests);
    }
    function loadProfile(key){
        if(!key)return;
        const previous=currentKey;
        $('.icue-profile-row').removeClass('active').filter(function(){return $(this).attr('data-key')===key;}).addClass('active');
        const activateAndLoad=()=>{
            status('Applying selected profile to hardware…');
            activateHardwareProfile(key).done(()=>{loadProfileData(key);status('Selected profile applied to hardware.');}).fail(x=>{
                toast.warning((x&&x.responseText)||'Unable to apply selected profile to hardware');
                status('Profile activation failed.');
            });
        };
        if(previous&&previous!==key&&liveDirty){
            status('Clearing previous live edit…');
            resetRuntimeProfile(previous,true).always(()=>{liveDirty=false;$('#icueResetLive').prop('disabled',true);activateAndLoad();});
            return;
        }
        activateAndLoad();
    }
    function resetLivePreview(){
        if(!currentKey)return;
        const key=currentKey;status('Resetting live preview…');
        resetRuntimeProfile(key,false).done(()=>{liveDirty=false;$('#icueResetLive').prop('disabled',true);loadProfileData(key);status('Live preview reset · imported source restored.');}).fail(x=>{toast.warning(x.responseText||'Unable to reset live preview');status('Live preview reset failed.');});
    }
    function selectLayer(target,index){currentTarget=target;currentLayer=target.layer;$('.icue-layer-row').removeClass('active').filter('[data-i="'+index+'"]').addClass('active');$('#icueLayerTitle').text(layerLabel(currentLayer,index)+' · '+(currentLayer.effect||'unknown'));$('#icueLayerEnabled').prop('checked',!!currentLayer.enabled);$('#icueLayerName').val(layerLabel(currentLayer,index));populateEffects();$('#icueSelectionMode').val((currentLayer.selectionMode||'').toLowerCase()==='allleds'?'AllLeds':'ConcreteLeds');$('#icueParameters').val(JSON.stringify(currentLayer.parameters||{},null,2));renderEffectControls();renderLEDs();editorSourceTrace();updateEffectClock();status('Live editor ready · imported source remains unchanged.');}
    function populateEffects(){const found=new Set(commonEffects);workspaceLayers(currentData||{}).forEach(x=>found.add(x.layer.effect));const s=$('#icueEffectType').empty();[...found].filter(Boolean).sort().forEach(x=>s.append($('<option>').val(x).text(x)));s.val(currentLayer.effect);}
    function firstPresentKey(keys,p){for(const k of keys){if(Object.prototype.hasOwnProperty.call(authoredParameters(),k)||Object.prototype.hasOwnProperty.call(p,k))return k;}return keys[0];}
    function renderEffectControls(){
        const box=$('#icueEffectControls').empty(),p=Object.assign({},currentLayer.parameters||{}),auth=authoredParameters(),used=new Set();
        Object.keys(auth).forEach(k=>{const v=authoredValue(k);if(v!==undefined)p[k]=v;});
        const schema=(schemas[String(currentLayer.effect||'').toLowerCase()]||[colours,speed,direction,duration]).slice();
        // Anything iCUE authored but the effect registry does not yet understand is
        // still surfaced. This is the calibration bench for the per-effect pass.
        Object.keys(auth).forEach(k=>{if(['color','colors','transitions','background'].includes(k))return;if(schema.some(f=>f.keys.includes(k)))return;schema.push(field(k,'Authored · '+k,boolKeys.has(k)?'bool':'number'));});
        schema.forEach(f=>{
            const key=firstPresentKey(f.keys,p);used.add(key);
            if(f.type==='colors'){
                const dc=authoredColors(),count=Math.max(String(currentLayer.effect||'').toLowerCase()==='staticlighting'?1:2,dc.length),cs=Array.from({length:count},(_,n)=>dc[n]||dc[0]||'#000000');
                cs.forEach((c,n)=>{const col=$('<div class="col-md-6"></div>');col.append($('<label class="sentence-text"></label>').text(f.label+(count>1?' '+(n+1):'')));const inp=$('<input type="color" class="form-control form-control-color icue-param-color">').attr('data-key',key).attr('data-n',n).val(c);col.append(inp);box.append(col);});
                return;
            }
            if(f.type==='bool'){
                const col=$('<div class="col-md-4"></div>'),wrap=$('<label class="system-toggle compact"></label>'),inp=$('<input type="checkbox" class="icue-param-bool">').attr('data-key',key).prop('checked',!!p[key]);wrap.append(inp,$('<span class="toggle-track"></span>'),$('<span class="ms-2"></span>').text(f.label));col.append(wrap);box.append(col);return;
            }
            const col=$('<div class="col-md-4"></div>');col.append($('<label class="sentence-text"></label>').text(f.label));const inp=$('<input type="number" step="any" class="form-control system-input icue-param-number">').attr('data-key',key);if(p[key]!==undefined&&!Array.isArray(p[key]))inp.val(p[key]);const raw=auth[key]&&auth[key].rawValues;if(raw&&raw.length)inp.attr('title','Authored: '+raw.join(' · '));col.append(inp);box.append(col);
        });
        const randomKey=Object.prototype.hasOwnProperty.call(auth,'isRandom')?'isRandom':'random';
        if(!used.has(randomKey)){const col=$('<div class="col-12"></div>'),wrap=$('<label class="system-toggle compact"></label>'),inp=$('<input type="checkbox" class="icue-param-bool">').attr('data-key',randomKey).prop('checked',!!p[randomKey]);wrap.append(inp,$('<span class="toggle-track"></span>'),$('<span class="ms-2">Random</span>'));col.append(wrap);box.append(col);}
    }
    function selectedGroupKind(){const v=String($('#icueDeviceCanvas').val()||'');return v.startsWith('group:')?v.slice(6):'';}
    function selectedDevices(){const kind=selectedGroupKind();if(kind)return detectedDevices.filter(d=>d.kind===kind);const i=parseInt($('#icueDeviceCanvas').val(),10);return Number.isFinite(i)&&detectedDevices[i]?[detectedDevices[i]]:[];}
    function selectedDevice(){return selectedDevices()[0]||null;}
    function selectedTargetLabel(){const kind=selectedGroupKind(),ds=selectedDevices();if(kind==='qx')return 'All QX Fans';if(kind==='memory')return 'All Memory';return ds[0]?ds[0].name:'device';}
    function mapperCapable(d){return !selectedGroupKind()&&!!d&&((d.kind==='qx'&&d.leds===34)||(d.kind==='titan'&&d.leds===20)||(d.kind==='memory'&&d.leds===6));}
    const qxLinkedStrips=[[3,4,5,6],[9,10,11,0],[15,16,17,18],[21,22,23,12]];
    function memoryPositionLabel(local,count){if(count<=1)return 'top';if(local===0)return 'top';if(local===count-1)return 'bottom';return 'top→bottom '+(local+1)+'/'+count;}
    function qxPhysicalLabel(local){if(local>=0&&local<=11)return 'front outer · clockwise';if(local>=12&&local<=23)return 'rear outer · +12 mirror';return 'inner output · physical position pending';}
    function qxStripLabels(local){const names=['front side A','front side B','rear side A','rear side B'];return qxLinkedStrips.map((g,i)=>g.includes(local)?names[i]:null).filter(Boolean);}
    function mapperOrdinal(d){if(!d)return 0;if(d.kind==='qx')return d.qxOrdinal||0;if(d.kind==='memory')return d.memoryOrdinal||0;return 0;}
    function mapperLedCount(d){return d?(d.kind==='titan'?20:(d.kind==='memory'?6:34)):34;}
    function loadMappingProbe(){return $.getJSON('/api/icue/mapping-probe').then(r=>{const p=r&&r.probe;if(!p)return;mappingObservations=(r&&r.observations)||[];probeLocal=Number.isFinite(Number(p.localLed))?Number(p.localLed):0;$('#icueTopologyProbe').prop('checked',!!p.enabled);$('#icueProbePanel').toggle(!!p.enabled);if(p.enabled){const idx=detectedDevices.findIndex(d=>d.kind===p.kind&&mapperOrdinal(d)===Number(p.deviceOrdinal));if(idx>=0)$('#icueDeviceCanvas').val(String(idx));}renderLEDs();if(p.enabled){const d=selectedDevice();$('#icueProbeStatus').text('Live raw mapper · '+(d?d.name:String(p.kind).toUpperCase())+' · raw LED '+probeLocal+' purple · all other LEDs blue');}});}
    function applyMappingProbe(){
        const d=selectedDevice(),enabled=$('#icueTopologyProbe').prop('checked')&&mapperCapable(d),max=mapperLedCount(d);
        if(probeLocal>=max)probeLocal=max-1;if(probeLocal<0)probeLocal=0;
        const body={enabled:!!enabled,kind:d&&mapperCapable(d)?d.kind:'qx',deviceOrdinal:mapperOrdinal(d),localLed:probeLocal};
        if(enabled)$('#icueProbeStatus').text('Sending raw mapper · '+d.name+' · raw LED '+probeLocal+'…');else $('#icueProbeStatus').text('Mapper off · profile playback unchanged.');
        return $.ajax({url:'/api/icue/mapping-probe',type:'POST',contentType:'application/json',data:JSON.stringify(body),success:r=>{mappingObservations=(r&&r.observations)||mappingObservations;const p=r&&r.probe;if(p&&p.enabled)$('#icueProbeStatus').text('Backend accepted · '+d.name+' · raw LED '+p.localLed+' · generation '+p.generation+' · check hardware override log for output confirmation');else $('#icueProbeStatus').text('Mapper off · profile playback unchanged.');updateProbeLabel();},error:x=>{toast.warning(x.responseText||'Unable to update LED mapping probe');$('#icueProbeStatus').text('Mapper update failed.');}});
    }
    function logicalFor(d,local){if(!d)return null;if(d.kind==='qx')return (d.qxOrdinal||0)*42+local;if(d.kind==='titan')return 756+local;if(d.kind==='memory')return (d.memoryOrdinal||0)*d.leds+local;return null;}
    function localSelected(local){if(!currentLayer)return false;if((currentLayer.selectionMode||'').toLowerCase()==='allleds')return true;const ds=selectedDevices();if(!ds.length)return false;const have=new Set((currentLayer.leds||[]).map(x=>{const m=String(x).match(/(\d+)\s*$/);return m?+m[1]:null;}).filter(x=>x!==null));return ds.every(d=>{const logical=logicalFor(d,local);return logical!==null&&have.has(logical);});}
    function currentObservation(d){return mappingObservations.find(x=>x.kind===d.kind&&Number(x.deviceOrdinal)===mapperOrdinal(d)&&Number(x.localLed)===probeLocal);}
    function syncMapperDeviceUI(d){
        const kind=d&&d.kind;
        $('#icueSelectionActions').toggle(!$('#icueTopologyProbe').prop('checked'));
        $('#icueQXObservation').toggle(kind==='qx'&&$('#icueTopologyProbe').prop('checked'));
        if(kind==='qx'){
            $('#icueMapperDescription').text('— measured QX: front outer 0–11 clockwise; rear outer 12–23 at +12. Side strips mirror ring outputs.');
            $('#icueMapperHint').text('Linked strips: front 3–6 + 9,10,11,0 · rear 15–18 + 21,22,23,12. Outputs 24–33 keep their existing inner projection pending a dedicated physical capture.');
        }else if(kind==='memory'){
            $('#icueMapperDescription').text('— measured memory orientation: raw LED 0 is top and increasing raw IDs flow toward the bottom.');
            $('#icueMapperHint').text((d.locator||d.name)+' · six-LED hardware measured 0→5 top→bottom; other LED counts use the same orientation as an inferred fallback.');
        }else if(kind==='titan'){
            $('#icueMapperDescription').text('— TITAN 280 is already measured; this raw probe is a hardware cross-check.');
            $('#icueMapperHint').text('TITAN raw outputs 0–19 retain the measured iCUE zone mapping.');
        }else{
            $('#icueMapperDescription').text('— select a QX, TITAN or six-LED DIMM to identify raw physical LED order.');
            $('#icueMapperHint').text('This device is not part of the current mapping pass.');
        }
    }
    function ledButton(d,local,extraClass,label){
        const selected=localSelected(local);
        let title=d.name+' · '+(d.kind==='qx'?qxPhysicalLabel(local):('physical position '+(local+1)));
        if(d.kind==='titan'&&d.leds===20)title=d.name+' · measured iCUE zone '+titan280Zones[local];
        if(d.kind==='memory')title=d.name+' · '+(d.locator||('module '+((d.memoryOrdinal||0)+1)))+' · '+memoryPositionLabel(local,d.leds);
        if(d.kind==='qx'&&d.leds===34){const strips=qxStripLabels(local);if(strips.length)title+=' · linked to '+strips.join(' + ');}
        const b=$('<button type="button" class="icue-led"></button>').addClass(extraClass||'').attr({'title':title,'aria-label':title});
        if(label)b.text(label);
        if(selected)b.addClass('selected').css('background-color',profileColor());
        if($('#icueTopologyProbe').prop('checked')&&mapperCapable(d)&&local===probeLocal)b.addClass('probe');
        b.on('click',()=>{if($('#icueTopologyProbe').prop('checked')&&mapperCapable(d)){probeLocal=local;renderLEDs();applyMappingProbe();}else toggleLED(local);});
        return b;
    }
    function placeFanLED(stage,d,local,angleDeg,radius,cls){
        const a=(angleDeg-90)*Math.PI/180,x=50+Math.cos(a)*radius,y=50+Math.sin(a)*radius;
        const b=ledButton(d,local,'fan-dot '+(cls||''),'').css({left:x+'%',top:y+'%'});stage.append(b);
    }
    function renderQXPhysical(map,d){
        map.removeClass('linear').addClass('physical qx-physical');
        const wrap=$('<div class="qx-layout"></div>');
        ['front','rear'].forEach((face,faceIndex)=>{
            const stage=$('<div class="icue-fan-stage"></div>').attr('aria-label',face+' face of '+d.name);
            stage.append($('<div class="fan-face-label"></div>').text(face==='front'?'Front':'Rear'));
            const base=faceIndex*12;
            // Measured outer ring: raw 1 is approximately 5 o'clock on the front and IDs increase clockwise.
            // Rear uses the measured +12 mirror. Raw IDs stay internal; the editor exposes physical positions only.
            const origin=150-30; // local 1 = 150° (5 o'clock), therefore local 0 = 120°.
            for(let j=0;j<12;j++)placeFanLED(stage,d,base+j,origin+j*30,40,'outer');
            // Inner outputs retain the existing projection until their exact physical capture is completed.
            const inner=face==='front'?[24,25,26,27,28,29]:[30,31,32,33];
            inner.forEach((local,j)=>placeFanLED(stage,d,local,(360/inner.length)*j,23,'inner'));
            const stripGroups=face==='front'?[[3,4,5,6],[9,10,11,0]]:[[15,16,17,18],[21,22,23,12]];
            stripGroups.forEach((group,side)=>{
                const bar=$('<div class="qx-side-strip"></div>').addClass(side===0?'strip-left':'strip-right');
                group.forEach(local=>{
                    const seg=ledButton(d,local,'strip-segment','');
                    bar.append(seg);
                });
                stage.append(bar);
            });
            wrap.append(stage);
        });
        map.append(wrap);
    }
    function renderMemoryPhysical(map,d){
        map.removeClass('linear').addClass('physical memory-physical');
        const stick=$('<div class="memory-stick"></div>').attr('aria-label',(d.locator||d.name)+' physical LED positions');
        for(let i=0;i<d.leds;i++)stick.append(ledButton(d,i,'memory-dot',''));
        map.append(stick);
    }
    function renderLEDs(){
        const map=$('#icueLedMap').empty().removeClass('linear physical qx-physical memory-physical'),d=selectedDevice();if(!d){$('#icueLedCount').text('No RGB device selected');return;}let count=0;
        for(let i=0;i<d.leds;i++)if(localSelected(i))count++;
        const diagnostic=$('#icueTopologyProbe').prop('checked');
        if(!diagnostic&&d.kind==='qx'&&d.leds===34)renderQXPhysical(map,d);
        else if(!diagnostic&&d.kind==='memory')renderMemoryPhysical(map,d);
        else{
            map.addClass('linear');
            for(let i=0;i<d.leds;i++)map.append(ledButton(d,i,'',diagnostic?String(i):''));
        }
        $('#icueLedCount').text(count+' / '+d.leds+' physical positions on '+selectedTargetLabel());$('#icueSelectAll').text('Include '+selectedTargetLabel());$('#icueSelectNone').text('Exclude '+selectedTargetLabel());$('#icueTopologyProbe').closest('label').toggle(mapperCapable(d));syncMapperDeviceUI(d);updateProbeLabel();editorSourceTrace();
    }
    function updateProbeLabel(){
        const d=selectedDevice();if(!mapperCapable(d))return;const max=mapperLedCount(d);if(probeLocal>=max)probeLocal=max-1;
        $('#icueProbeLed').text('Raw LED '+probeLocal+' of '+(max-1));const obs=currentObservation(d);
        if(d.kind==='titan')$('#icueProbeZone').text('TITAN 280 · measured iCUE zone '+titan280Zones[probeLocal]+' · raw output '+probeLocal);
        else if(d.kind==='memory')$('#icueProbeZone').text((d.locator||d.name)+' · '+memoryPositionLabel(probeLocal,d.leds)+' · measured direction 0→'+(d.leds-1)+' top→bottom');
        else {const strips=qxStripLabels(probeLocal),measured=probeLocal<=23?qxPhysicalLabel(probeLocal):null;$('#icueProbeZone').text(obs?[(obs.face||'?'),(obs.ring||'?'),(obs.position||'?')].join(' · '):(measured?measured+(strips.length?' · linked '+strips.join(' + '):''):'Inner output · record physical position'));}
        $('#icueMapFace').val(obs&&obs.face||'');$('#icueMapRing').val(obs&&obs.ring||'');$('#icueMapPosition').val(obs&&obs.position||'');
    }
    function recordMappingObservation(){
        const d=selectedDevice();if(!d||d.kind!=='qx')return;
        const body={enabled:$('#icueTopologyProbe').prop('checked'),kind:'qx',deviceOrdinal:mapperOrdinal(d),localLed:probeLocal,record:true,face:$('#icueMapFace').val(),ring:$('#icueMapRing').val(),position:$('#icueMapPosition').val().trim()};
        if(!body.face||!body.ring||!body.position){toast.warning('Record Front/Rear, Inner/Outer and physical position first.');return;}
        $.ajax({url:'/api/icue/mapping-probe',type:'POST',contentType:'application/json',data:JSON.stringify(body),success:r=>{mappingObservations=(r&&r.observations)||mappingObservations;updateProbeLabel();$('#icueProbeStatus').text('Recorded raw LED '+probeLocal+' · '+body.face+' · '+body.ring+' · '+body.position);},error:x=>toast.warning(x.responseText||'Unable to record mapper observation')});
    }
    function ledIdsSet(){return new Set((currentLayer.leds||[]).map(x=>{const m=String(x).match(/(\d+)\s*$/);return m?+m[1]:null;}).filter(x=>x!==null));}
    function toggleLED(local){const ds=selectedDevices();if(!ds.length)return;const logicals=ds.map(d=>logicalFor(d,local)).filter(x=>x!==null);if(!logicals.length){toast.warning('Concrete LED IDs for this device topology are not mapped yet; All LEDs is safe.');return;}let ids=(currentLayer.leds||[]).map(String);if((currentLayer.selectionMode||'').toLowerCase()==='allleds')ids=[];const have=new Set(ids.map(x=>{const m=x.match(/(\d+)\s*$/);return m?+m[1]:null;}).filter(x=>x!==null)),allOn=logicals.every(x=>have.has(x));if(allOn)ids=ids.filter(x=>{const m=String(x).match(/(\d+)\s*$/);return !m||!logicals.includes(+m[1]);});else logicals.forEach(x=>{if(!have.has(x)){ids.push('LedId::'+x);have.add(x);}});currentLayer.selectionMode='ConcreteLeds';currentLayer.leds=ids;$('#icueSelectionMode').val('ConcreteLeds');renderLEDs();apply({selectionMode:'ConcreteLeds',leds:ids});}
    function setDeviceSelection(include){const ds=selectedDevices();if(!ds.length)return;let ids=(currentLayer.leds||[]).map(String);if((currentLayer.selectionMode||'').toLowerCase()==='allleds')ids=[];const target=new Set();ds.forEach(d=>{for(let i=0;i<d.leds;i++){const x=logicalFor(d,i);if(x!==null)target.add(x);}});ids=ids.filter(x=>{const m=String(x).match(/(\d+)\s*$/);return !m||!target.has(+m[1]);});if(include)target.forEach(x=>ids.push('LedId::'+x));currentLayer.selectionMode='ConcreteLeds';currentLayer.leds=ids;$('#icueSelectionMode').val('ConcreteLeds');renderLEDs();apply({selectionMode:'ConcreteLeds',leds:ids});}
    function apply(changes){if(!currentTarget||!currentKey||applying)return;const body={deviceIndex:currentTarget.deviceIndex,channelIndex:currentTarget.channelIndex,layerIndex:currentTarget.layerIndex,lightingLink:currentTarget.lightingLink};Object.assign(body,changes);applying=true;status('Applying live…');$.ajax({url:'/api/icue/profile?key='+encodeURIComponent(currentKey),type:'POST',contentType:'application/json',data:JSON.stringify(body),success:r=>{currentLayer=Object.assign(currentLayer,r.layer||{});liveDirty=true;$('#icueResetLive').prop('disabled',false);status('Live edit applied · source .cueprofile unchanged.');renderLEDs();},error:x=>{toast.warning(x.responseText||'Unable to apply live edit');status('Live edit failed.');},complete:()=>applying=false});}
    function applyParam(key,val){currentLayer.parameters=currentLayer.parameters||{};currentLayer.parameters[key]=val;$('#icueParameters').val(JSON.stringify(currentLayer.parameters,null,2));updateEffectClock();apply({parameters:{[key]:val}});}

    $('#icueProfileForm').on('submit',function(e){e.preventDefault();const f=$('#icueProfileFile')[0].files[0];if(!f){toast.warning('Choose a .cueprofile first');return;}const fd=new FormData();fd.append('profileFile',f);$.ajax({url:'/api/icue/upload',type:'POST',data:fd,processData:false,contentType:false,success:r=>{toast.success(r.message||'Profile imported');refreshProfiles(r.profile&&r.profile.key);},error:x=>toast.warning(x.responseText||'Import failed')});});
    $('#icueDeviceCanvas').on('change',function(){if(selectedGroupKind()&&$('#icueTopologyProbe').prop('checked')){$('#icueTopologyProbe').prop('checked',false);$('#icueProbePanel').hide();applyMappingProbe();}renderLEDs();if($('#icueTopologyProbe').prop('checked'))applyMappingProbe();});$('#icueSelectionMode').on('change',function(){currentLayer.selectionMode=this.value;renderLEDs();apply({selectionMode:this.value});});$('#icueLayerEnabled').on('change',function(){currentLayer.enabled=this.checked;apply({enabled:this.checked});});
    $('#icueLayerName').on('change',function(){currentLayer.name=this.value;apply({name:this.value});});$('#icueEffectType').on('change',function(){currentLayer.effect=this.value;renderEffectControls();apply({effect:this.value});});
    $('#icueEffectControls').on('change','.icue-param-number',function(){if(this.value!=='')applyParam($(this).data('key'),Number(this.value));}).on('change','.icue-param-color',function(){const key=$(this).data('key'),inputs=$('.icue-param-color[data-key="'+key+'"]'),colors=inputs.map((_,x)=>hexToRGB(x.value)).get();applyParam(key,colors);renderLEDs();}).on('change','.icue-param-bool',function(){applyParam($(this).data('key'),this.checked);});
    $('#icueApplyParameters').on('click',function(){try{const p=JSON.parse($('#icueParameters').val()||'{}');currentLayer.parameters=p;renderEffectControls();renderLEDs();apply({parameters:p});}catch(e){toast.warning('Parameters must be valid JSON');}});
    $('#icueSelectAll').on('click',()=>setDeviceSelection(true));$('#icueSelectNone').on('click',()=>setDeviceSelection(false));$('#icueTopologyProbe').on('change',function(){$('#icueProbePanel').toggle(this.checked);$('#icueSelectionActions').toggle(!this.checked);renderLEDs();applyMappingProbe();});$('#icueProbePrev').on('click',()=>{const d=selectedDevice(),n=mapperLedCount(d);probeLocal=(probeLocal+n-1)%n;renderLEDs();applyMappingProbe();});$('#icueProbeNext').on('click',()=>{const d=selectedDevice(),n=mapperLedCount(d);probeLocal=(probeLocal+1)%n;renderLEDs();applyMappingProbe();});$('#icueMapRecord').on('click',recordMappingObservation);
    $('#icueResetLive').on('click',resetLivePreview);
    $(window).on('pagehide',function(){if(liveDirty&&currentKey)fetch('/api/icue/profile?key='+encodeURIComponent(currentKey),{method:'DELETE',keepalive:true});});
    function layerAction(action){if(!currentTarget||!currentTarget.lightingLink){toast.warning('Layer structure editing currently applies to authored Lighting Link layers.');return;}const body={action:action,lightingLink:true,layerIndex:currentTarget.layerIndex,deviceIndex:currentTarget.deviceIndex,channelIndex:currentTarget.channelIndex};$.ajax({url:'/api/icue/profile?key='+encodeURIComponent(currentKey),type:'POST',contentType:'application/json',data:JSON.stringify(body),success:()=>loadProfile(currentKey),error:x=>toast.warning(x.responseText||'Unable to edit layer structure')});}
    $('#icueAddLayer').on('click',()=>layerAction('add'));$('#icueDuplicateLayer').on('click',()=>layerAction('duplicate'));$('#icueMoveUp').on('click',()=>layerAction('up'));$('#icueMoveDown').on('click',()=>layerAction('down'));$('#icueDeleteLayer').on('click',()=>layerAction('delete'));
    $('#icueSlowScaleRange,#icueSlowScale').on('input change',function(){const v=Number(this.value);if(Number.isFinite(v))applyTimingCalibration(v,timingCalibration.fast);});
    $('#icueFastScaleRange,#icueFastScale').on('input change',function(){const v=Number(this.value);if(Number.isFinite(v))applyTimingCalibration(timingCalibration.slow,v);});
    $('#icueTimingReset').on('click',()=>applyTimingCalibration(1.5,0.5));
    $('.icue-rain-method').on('click',function(){applyRainMethod($(this).data('method'));});
    $.when(refreshDevices().then(loadMappingProbe),loadTimingCalibration(),loadRainMethod()).always(()=>{refreshProfiles();});
});
