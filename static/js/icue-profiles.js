$(function () {
    let currentKey = '';
    let currentData = null;
    let currentTarget = null;
    let currentLayer = null;
    let applying = false;
    let probeLocal = 0;
    const qxTopology = [
        {key:'front-outer', label:'Front Outer Ring', face:'front', region:'outer', start:0, count:12},
        {key:'back-outer', label:'Back Outer Ring', face:'rear', region:'outer', start:12, count:12},
        {key:'front-inner', label:'Front Inner Ring', face:'front', region:'inner', start:24, count:6},
        {key:'back-inner', label:'Back Inner Ring', face:'rear', region:'inner', start:30, count:4}
    ];

    function esc(s) { return $('<div>').text(s == null ? '' : String(s)).html(); }
    function layerLabel(l, i) { return (l.name && l.name.trim()) || l.effectName || l.effect || ('Layer ' + (i + 1)); }
    function status(text) { $('#icueLiveStatus').text(text); }

    function refreshProfiles(selectKey) {
        $.getJSON('/api/icue/profiles', function (r) {
            $('#icueProfileDirectory').text(r.directory ? 'Stored in: ' + r.directory : '');
            const list = $('#icueProfileList').empty();
            (r.profiles || []).forEach(function (p) {
                const row = $('<div class="settings-row icue-profile-row"></div>').attr('data-key', p.key);
                row.append($('<span class="settings-label text-ellipsis"></span>').text(p.name || p.fileName));
                row.append($('<small></small>').text(p.layers + ' layers'));
                row.on('click', function () { loadProfile(p.key); });
                list.append(row);
            });
            if (!(r.profiles || []).length) list.append('<div class="settings-row"><span class="settings-label">No iCUE profiles imported</span></div>');
            Object.keys(r.failures || {}).forEach(function (f) { list.append($('<div class="settings-row"></div>').text(f + ': ' + r.failures[f])); });
            if (selectKey) loadProfile(selectKey);
        });
    }

    function flattenLayers(data) {
        const out = [];
        const p = data.profile || {};
        (p.devices || []).forEach(function (d, di) {
            (d.channels || []).forEach(function (c, ci) {
                (c.layers || []).forEach(function (l, li) { out.push({layer:l, deviceIndex:di, channelIndex:ci, layerIndex:li, lightingLink:false, group:'Device ' + (di+1) + ' · Ch ' + c.index}); });
            });
        });
        (data.lightingLinkLayers || []).forEach(function (l, li) { out.push({layer:l, deviceIndex:0, channelIndex:0, layerIndex:li, lightingLink:true, group:'Lighting Link'}); });
        return out;
    }

    function loadProfile(key) {
        currentKey = key;
        $.getJSON('/api/icue/profile?key=' + encodeURIComponent(key), function (data) {
            currentData = data;
            $('.icue-profile-row').removeClass('active').filter('[data-key="' + CSS.escape(key) + '"]').addClass('active');
            $('#icueEmpty').hide(); $('#icueWorkspace').show();
            const p = data.profile || {};
            $('#icueProfileTitle').text(p.name || data.fileName || key);
            $('#icueMetaFile').text(data.fileName || '—');
            $('#icueMetaDevices').text((p.summary || {}).deviceEntries ?? (p.devices || []).length);
            $('#icueMetaChannels').text((p.summary || {}).channels ?? '—');
            $('#icueMetaLayers').text((p.summary || {}).layers ?? '—');
            const layers = flattenLayers(data), list = $('#icueLayerList').empty();
            layers.forEach(function (x, i) {
                const row = $('<div class="settings-row icue-layer-row"></div>').attr('data-i', i);
                row.append($('<span class="settings-label text-ellipsis"></span>').text(layerLabel(x.layer, i)));
                row.append($('<small></small>').text(x.layer.effect || 'unknown'));
                row.on('click', function () { selectLayer(x, i); });
                list.append(row);
            });
            if (layers.length) selectLayer(layers[0], 0);
        }).fail(function (xhr) { toast.warning(xhr.responseText || 'Unable to load iCUE profile'); });
    }

    function selectLayer(target, index) {
        currentTarget = target; currentLayer = target.layer;
        $('.icue-layer-row').removeClass('active').filter('[data-i="' + index + '"]').addClass('active');
        $('#icueLayerTitle').text(target.group + ' · ' + layerLabel(currentLayer, index) + ' · ' + (currentLayer.effect || 'unknown'));
        $('#icueLayerEnabled').prop('checked', !!currentLayer.enabled);
        $('#icueSelectionMode').val((currentLayer.selectionMode || '').toLowerCase() === 'allleds' ? 'AllLeds' : 'ConcreteLeds');
        const p = currentLayer.parameters || {};
        $('#icueRandomTest').prop('checked', !!(p.random || p.isRandom));
        setNumber('#icueSpeed', p.speed); setNumber('#icueDuration', p.duration); setNumber('#icueDirection', p.direction);
        const colors = getColors(p); $('#icueColor1').val(colors[0]); $('#icueColor2').val(colors[1]);
        $('#icueParameters').val(JSON.stringify(p, null, 2));
        renderLEDs(); status('Changes apply live and reset on restart/re-upload.');
    }

    function setNumber(id, value) { $(id).val((typeof value === 'number' || /^-?\d+(\.\d+)?$/.test(String(value || ''))) ? value : ''); }
    function colorToHex(v) {
        const m = String(v || '').match(/rgba?\(\s*([\d.]+)[ ,]+([\d.]+)[ ,]+([\d.]+)/i); if (!m) return '#000000';
        let a = [Number(m[1]), Number(m[2]), Number(m[3])]; if (Math.max.apply(null,a) <= 1.001) a = a.map(x => x*255);
        return '#' + a.map(x => Math.max(0,Math.min(255,Math.round(x))).toString(16).padStart(2,'0')).join('');
    }
    function hexToRGB(h) { return 'rgb(' + [1,3,5].map(i => (parseInt(h.slice(i,i+2),16)/255).toFixed(6).replace(/0+$/,'').replace(/\.$/,'')).join(' ') + ')'; }
    function getColors(p) {
        let xs = Array.isArray(p.colors) ? p.colors : (p.colors ? [p.colors] : []);
        if (!xs.length && p.color) xs=[p.color];
        return [colorToHex(xs[0]), colorToHex(xs[1] || xs[0])];
    }

    function localSelected(local) {
        if (!currentLayer) return false;
        if ((currentLayer.selectionMode || '').toLowerCase() === 'allleds') return true;
        const fan = parseInt($('#icueFan').val(),10), logical = fan * 42 + local;
        return (currentLayer.leds || []).some(x => { const m=String(x).match(/(\d+)\s*$/); return m && parseInt(m[1],10)===logical; });
    }
    // Confirmed QX segment boundaries from OpenLinkHub. Angular start/direction remain to verify.
    function qxSegment(local) { return qxTopology.find(z => local >= z.start && local < z.start + z.count); }
    function topologySlot(local) {
        const z=qxSegment(local);
        if(!z)return {face:'front',x:50,y:50,region:'unknown',label:'Unknown',segmentIndex:0,segmentCount:1};
        const n=local-z.start, radius=z.region==='outer'?39:20;
        const a=(-90+(360*n/z.count))*Math.PI/180;
        return {face:z.face,x:50+radius*Math.cos(a),y:50+radius*Math.sin(a),region:z.region,label:z.label,segmentIndex:n,segmentCount:z.count};
    }
    function facePanel(name,label) {
        const face=$('<div class="icue-face"></div>').attr('data-face',name);
        face.append($('<div class="icue-face-title"></div>').text(label));
        const stage=$('<div class="icue-fan-stage"></div>');
        stage.append('<div class="icue-fan-ring"></div><div class="icue-fan-ring inner"></div><div class="icue-sidebar left"></div><div class="icue-sidebar right"></div>');
        face.append(stage); return {face:face,stage:stage};
    }
    function renderLEDs() {
        const map=$('#icueLedMap').empty(), front=facePanel('front','Front'), rear=facePanel('rear','Rear'); let count=0;
        map.append(front.face,rear.face);
        for(let i=0;i<34;i++) {
            const selected=localSelected(i), pos=topologySlot(i); if(selected) count++;
            const b=$('<button type="button" class="icue-led"></button>').text(i).attr({'aria-label':'Local LED '+i+' · '+pos.label,'title':'Local LED '+i+' · '+pos.label+' · group position '+(pos.segmentIndex+1)+'/'+pos.segmentCount+' · angular order still to verify'});
            b.css({left:pos.x+'%',top:pos.y+'%'}); if(selected)b.addClass('selected'); if($('#icueTopologyProbe').prop('checked')&&i===probeLocal)b.addClass('probe');
            b.on('click',()=>toggleLED(i)); (pos.face==='front'?front.stage:rear.stage).append(b);
        }
        $('#icueLedCount').text(count + ' / 34 LEDs on selected fan'); updateProbeLabel();
    }
    function updateProbeLabel(){
        const fan=parseInt($('#icueFan').val(),10)||0, z=qxSegment(probeLocal), n=z?probeLocal-z.start:0;
        $('#icueProbeLed').text('Local LED '+probeLocal+' · Logical '+(fan*42+probeLocal));
        $('#icueProbeZone').text(z?z.label+' · candidate order '+(n+1)+'/'+z.count:'Unknown zone');
        $('#icueProbeGroup').val(z?z.key:'front-outer');
    }
    function stepProbe(delta){ probeLocal=(probeLocal+delta+34)%34; renderLEDs(); }
    function selectProbeGroup(key){ const z=qxTopology.find(x=>x.key===key); if(!z)return; probeLocal=z.start; renderLEDs(); }
    function setFanSelection(mode) {
        if (!currentLayer) return;
        const fan=parseInt($('#icueFan').val(),10), lo=fan*42, hi=lo+33;
        let ids=(currentLayer.leds||[]).map(String).filter(x=>{const m=x.match(/(\d+)\s*$/); if(!m)return true; const n=parseInt(m[1],10); return n<lo||n>hi;});
        if(mode==='all') for(let i=0;i<34;i++) ids.push('LedId::' + (lo+i));
        currentLayer.selectionMode='ConcreteLeds'; currentLayer.leds=ids; $('#icueSelectionMode').val('ConcreteLeds'); renderLEDs(); apply({selectionMode:'ConcreteLeds', leds:ids});
    }
    function toggleLED(local) {
        const fan=parseInt($('#icueFan').val(),10), logical=fan*42+local; let ids=(currentLayer.leds||[]).map(String);
        if ((currentLayer.selectionMode||'').toLowerCase()==='allleds') { ids=[]; for(let i=0;i<34;i++) ids.push('LedId::'+(fan*42+i)); }
        let found=-1;
        ids.forEach((x,i)=>{const m=x.match(/(\d+)\s*$/); if(m&&parseInt(m[1],10)===logical)found=i;});
        if(found>=0) ids.splice(found,1); else ids.push('LedId::'+logical);
        currentLayer.selectionMode='ConcreteLeds'; currentLayer.leds=ids; $('#icueSelectionMode').val('ConcreteLeds'); renderLEDs(); apply({selectionMode:'ConcreteLeds', leds:ids});
    }

    function apply(changes) {
        if (!currentTarget || !currentKey || applying) return;
        const body={deviceIndex:currentTarget.deviceIndex,channelIndex:currentTarget.channelIndex,layerIndex:currentTarget.layerIndex,lightingLink:currentTarget.lightingLink};
        Object.assign(body,changes); applying=true; status('Applying live…');
        $.ajax({url:'/api/icue/profile?key='+encodeURIComponent(currentKey),type:'POST',contentType:'application/json',data:JSON.stringify(body),success:function(r){ currentLayer=Object.assign(currentLayer,r.layer||{}); status('Live edit applied · source .cueprofile unchanged.'); },error:function(xhr){toast.warning(xhr.responseText||'Unable to apply live edit');status('Live edit failed.');},complete:function(){applying=false;}});
    }

    $('#icueProfileForm').on('submit', function(e){ e.preventDefault(); const f=$('#icueProfileFile')[0].files[0]; if(!f){toast.warning('Choose a .cueprofile first');return;} const fd=new FormData();fd.append('profileFile',f);$('#btnICUEProfileUpload').prop('disabled',true);$.ajax({url:'/api/icue/upload',type:'POST',data:fd,processData:false,contentType:false,success:r=>{toast.success(r.message||'Profile imported');$('#icueProfileFile').val('');refreshProfiles(r.profile && r.profile.key ? r.profile.key : undefined);},error:x=>toast.warning(x.responseText||'Import failed'),complete:()=>$('#btnICUEProfileUpload').prop('disabled',false)}); });
    $('#icueFan').on('change',renderLEDs); $('#icueTopologyProbe').on('change',function(){ $('#icueProbePanel').toggle(this.checked); renderLEDs(); }); $('#icueProbePrev').on('click',()=>stepProbe(-1)); $('#icueProbeNext').on('click',()=>stepProbe(1)); $('#icueProbeGroup').on('change',function(){selectProbeGroup(this.value);}); $('#icueSelectAll').on('click',()=>setFanSelection('all')); $('#icueSelectNone').on('click',()=>setFanSelection('none'));
    $('#icueLayerEnabled').on('change',function(){currentLayer.enabled=this.checked;apply({enabled:this.checked});});
    $('#icueSelectionMode').on('change',function(){currentLayer.selectionMode=this.value;renderLEDs();apply({selectionMode:this.value});});
    $('#icueRandomTest').on('change',function(){const k=('isRandom' in (currentLayer.parameters||{}))?'isRandom':'random';currentLayer.parameters=currentLayer.parameters||{};currentLayer.parameters[k]=this.checked;$('#icueParameters').val(JSON.stringify(currentLayer.parameters,null,2));apply({parameters:{[k]:this.checked}});});
    [['#icueSpeed','speed'],['#icueDuration','duration'],['#icueDirection','direction']].forEach(x=>$(x[0]).on('change',function(){if(this.value==='')return;const v=Number(this.value);currentLayer.parameters=currentLayer.parameters||{};currentLayer.parameters[x[1]]=v;$('#icueParameters').val(JSON.stringify(currentLayer.parameters,null,2));apply({parameters:{[x[1]]:v}});}));
    $('#icueColor1,#icueColor2').on('change',function(){const colors=[hexToRGB($('#icueColor1').val()),hexToRGB($('#icueColor2').val())];currentLayer.parameters=currentLayer.parameters||{};currentLayer.parameters.colors=colors;$('#icueParameters').val(JSON.stringify(currentLayer.parameters,null,2));apply({parameters:{colors:colors}});});
    $('#icueApplyParameters').on('click',function(){try{const p=JSON.parse($('#icueParameters').val()||'{}');currentLayer.parameters=p;apply({parameters:p});}catch(e){toast.warning('Parameters must be valid JSON');}});

    function loadRain(){ $.getJSON('/api/icue/rain-calibration',v=>{['Slow','Normal','Fast'].forEach(k=>{$('#icueRain'+k).val(Math.round(v[k.toLowerCase()+'Ms']));$('#icueRain'+k+'Value').text(Math.round(v[k.toLowerCase()+'Ms'])+' ms');});}); }
    let rainTimer=null; $('#icueRainSlow,#icueRainNormal,#icueRainFast').on('input',function(){const id=this.id, label=id+'Value';$('#'+label).text(Math.round(Number(this.value))+' ms');clearTimeout(rainTimer);rainTimer=setTimeout(()=>{$.ajax({url:'/api/icue/rain-calibration',type:'POST',contentType:'application/json',data:JSON.stringify({slowMs:Number($('#icueRainSlow').val()),normalMs:Number($('#icueRainNormal').val()),fastMs:Number($('#icueRainFast').val())}),success:()=>$('#icueRainCalibrationStatus').text('Live Rain timing applied.'),error:x=>$('#icueRainCalibrationStatus').text(x.responseText||'Unable to apply timing')});},100);});

    refreshProfiles(); loadRain();
});
