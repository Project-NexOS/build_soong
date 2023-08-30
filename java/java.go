// Copyright 2015 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package java

// This file contains the module types for compiling Java for Android, and converts the properties
// into the flags and filenames necessary to pass to the Module.  The final creation of the rules
// is handled in builder.go

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"android/soong/bazel"
	"android/soong/bazel/cquery"
	"android/soong/remoteexec"
	"android/soong/ui/metrics/bp2build_metrics_proto"

	"github.com/google/blueprint"
	"github.com/google/blueprint/proptools"

	"android/soong/android"
	"android/soong/cc"
	"android/soong/dexpreopt"
	"android/soong/java/config"
	"android/soong/tradefed"
)

func init() {
	registerJavaBuildComponents(android.InitRegistrationContext)

	RegisterJavaSdkMemberTypes()
}

func registerJavaBuildComponents(ctx android.RegistrationContext) {
	ctx.RegisterModuleType("java_defaults", DefaultsFactory)

	ctx.RegisterModuleType("java_library", LibraryFactory)
	ctx.RegisterModuleType("java_library_static", LibraryStaticFactory)
	ctx.RegisterModuleType("java_library_host", LibraryHostFactory)
	ctx.RegisterModuleType("java_binary", BinaryFactory)
	ctx.RegisterModuleType("java_binary_host", BinaryHostFactory)
	ctx.RegisterModuleType("java_test", TestFactory)
	ctx.RegisterModuleType("java_test_helper_library", TestHelperLibraryFactory)
	ctx.RegisterModuleType("java_test_host", TestHostFactory)
	ctx.RegisterModuleType("java_test_import", JavaTestImportFactory)
	ctx.RegisterModuleType("java_import", ImportFactory)
	ctx.RegisterModuleType("java_import_host", ImportFactoryHost)
	ctx.RegisterModuleType("java_device_for_host", DeviceForHostFactory)
	ctx.RegisterModuleType("java_host_for_device", HostForDeviceFactory)
	ctx.RegisterModuleType("dex_import", DexImportFactory)
	ctx.RegisterModuleType("java_api_library", ApiLibraryFactory)
	ctx.RegisterModuleType("java_api_contribution", ApiContributionFactory)

	// This mutator registers dependencies on dex2oat for modules that should be
	// dexpreopted. This is done late when the final variants have been
	// established, to not get the dependencies split into the wrong variants and
	// to support the checks in dexpreoptDisabled().
	ctx.FinalDepsMutators(func(ctx android.RegisterMutatorsContext) {
		ctx.BottomUp("dexpreopt_tool_deps", dexpreoptToolDepsMutator).Parallel()
		// needs access to ApexInfoProvider which is available after variant creation
		ctx.BottomUp("jacoco_deps", jacocoDepsMutator).Parallel()
	})

	ctx.RegisterParallelSingletonType("logtags", LogtagsSingleton)
	ctx.RegisterParallelSingletonType("kythe_java_extract", kytheExtractJavaFactory)
}

func RegisterJavaSdkMemberTypes() {
	// Register sdk member types.
	android.RegisterSdkMemberType(javaHeaderLibsSdkMemberType)
	android.RegisterSdkMemberType(javaLibsSdkMemberType)
	android.RegisterSdkMemberType(javaBootLibsSdkMemberType)
	android.RegisterSdkMemberType(javaSystemserverLibsSdkMemberType)
	android.RegisterSdkMemberType(javaTestSdkMemberType)
}

var (
	// Supports adding java header libraries to module_exports and sdk.
	javaHeaderLibsSdkMemberType = &librarySdkMemberType{
		android.SdkMemberTypeBase{
			PropertyName: "java_header_libs",
			SupportsSdk:  true,
		},
		func(_ android.SdkMemberContext, j *Library) android.Path {
			headerJars := j.HeaderJars()
			if len(headerJars) != 1 {
				panic(fmt.Errorf("there must be only one header jar from %q", j.Name()))
			}

			return headerJars[0]
		},
		sdkSnapshotFilePathForJar,
		copyEverythingToSnapshot,
	}

	// Export implementation classes jar as part of the sdk.
	exportImplementationClassesJar = func(_ android.SdkMemberContext, j *Library) android.Path {
		implementationJars := j.ImplementationAndResourcesJars()
		if len(implementationJars) != 1 {
			panic(fmt.Errorf("there must be only one implementation jar from %q", j.Name()))
		}
		return implementationJars[0]
	}

	// Supports adding java implementation libraries to module_exports but not sdk.
	javaLibsSdkMemberType = &librarySdkMemberType{
		android.SdkMemberTypeBase{
			PropertyName: "java_libs",
		},
		exportImplementationClassesJar,
		sdkSnapshotFilePathForJar,
		copyEverythingToSnapshot,
	}

	snapshotRequiresImplementationJar = func(ctx android.SdkMemberContext) bool {
		// In the S build the build will break if updatable-media does not provide a full implementation
		// jar. That issue was fixed in Tiramisu by b/229932396.
		if ctx.IsTargetBuildBeforeTiramisu() && ctx.Name() == "updatable-media" {
			return true
		}

		return false
	}

	// Supports adding java boot libraries to module_exports and sdk.
	//
	// The build has some implicit dependencies (via the boot jars configuration) on a number of
	// modules, e.g. core-oj, apache-xml, that are part of the java boot class path and which are
	// provided by mainline modules (e.g. art, conscrypt, runtime-i18n) but which are not otherwise
	// used outside those mainline modules.
	//
	// As they are not needed outside the mainline modules adding them to the sdk/module-exports as
	// either java_libs, or java_header_libs would end up exporting more information than was strictly
	// necessary. The java_boot_libs property to allow those modules to be exported as part of the
	// sdk/module_exports without exposing any unnecessary information.
	javaBootLibsSdkMemberType = &librarySdkMemberType{
		android.SdkMemberTypeBase{
			PropertyName: "java_boot_libs",
			SupportsSdk:  true,
		},
		func(ctx android.SdkMemberContext, j *Library) android.Path {
			if snapshotRequiresImplementationJar(ctx) {
				return exportImplementationClassesJar(ctx, j)
			}

			// Java boot libs are only provided in the SDK to provide access to their dex implementation
			// jar for use by dexpreopting and boot jars package check. They do not need to provide an
			// actual implementation jar but the java_import will need a file that exists so just copy an
			// empty file. Any attempt to use that file as a jar will cause a build error.
			return ctx.SnapshotBuilder().EmptyFile()
		},
		func(ctx android.SdkMemberContext, osPrefix, name string) string {
			if snapshotRequiresImplementationJar(ctx) {
				return sdkSnapshotFilePathForJar(ctx, osPrefix, name)
			}

			// Create a special name for the implementation jar to try and provide some useful information
			// to a developer that attempts to compile against this.
			// TODO(b/175714559): Provide a proper error message in Soong not ninja.
			return filepath.Join(osPrefix, "java_boot_libs", "snapshot", "jars", "are", "invalid", name+jarFileSuffix)
		},
		onlyCopyJarToSnapshot,
	}

	// Supports adding java systemserver libraries to module_exports and sdk.
	//
	// The build has some implicit dependencies (via the systemserver jars configuration) on a number
	// of modules that are part of the java systemserver classpath and which are provided by mainline
	// modules but which are not otherwise used outside those mainline modules.
	//
	// As they are not needed outside the mainline modules adding them to the sdk/module-exports as
	// either java_libs, or java_header_libs would end up exporting more information than was strictly
	// necessary. The java_systemserver_libs property to allow those modules to be exported as part of
	// the sdk/module_exports without exposing any unnecessary information.
	javaSystemserverLibsSdkMemberType = &librarySdkMemberType{
		android.SdkMemberTypeBase{
			PropertyName: "java_systemserver_libs",
			SupportsSdk:  true,

			// This was only added in Tiramisu.
			SupportedBuildReleaseSpecification: "Tiramisu+",
		},
		func(ctx android.SdkMemberContext, j *Library) android.Path {
			// Java systemserver libs are only provided in the SDK to provide access to their dex
			// implementation jar for use by dexpreopting. They do not need to provide an actual
			// implementation jar but the java_import will need a file that exists so just copy an empty
			// file. Any attempt to use that file as a jar will cause a build error.
			return ctx.SnapshotBuilder().EmptyFile()
		},
		func(_ android.SdkMemberContext, osPrefix, name string) string {
			// Create a special name for the implementation jar to try and provide some useful information
			// to a developer that attempts to compile against this.
			// TODO(b/175714559): Provide a proper error message in Soong not ninja.
			return filepath.Join(osPrefix, "java_systemserver_libs", "snapshot", "jars", "are", "invalid", name+jarFileSuffix)
		},
		onlyCopyJarToSnapshot,
	}

	// Supports adding java test libraries to module_exports but not sdk.
	javaTestSdkMemberType = &testSdkMemberType{
		SdkMemberTypeBase: android.SdkMemberTypeBase{
			PropertyName: "java_tests",
		},
	}

	// Rule for generating device binary default wrapper
	deviceBinaryWrapper = pctx.StaticRule("deviceBinaryWrapper", blueprint.RuleParams{
		Command: `echo -e '#!/system/bin/sh\n` +
			`export CLASSPATH=/system/framework/$jar_name\n` +
			`exec app_process /$partition/bin $main_class "$$@"'> ${out}`,
		Description: "Generating device binary wrapper ${jar_name}",
	}, "jar_name", "partition", "main_class")
)

type ProguardSpecInfo struct {
	// If true, proguard flags files will be exported to reverse dependencies across libs edges
	// If false, proguard flags files will only be exported to reverse dependencies across
	// static_libs edges.
	Export_proguard_flags_files bool

	// TransitiveDepsProguardSpecFiles is a depset of paths to proguard flags files that are exported from
	// all transitive deps. This list includes all proguard flags files from transitive static dependencies,
	// and all proguard flags files from transitive libs dependencies which set `export_proguard_spec: true`.
	ProguardFlagsFiles *android.DepSet[android.Path]

	// implementation detail to store transitive proguard flags files from exporting shared deps
	UnconditionallyExportedProguardFlags *android.DepSet[android.Path]
}

var ProguardSpecInfoProvider = blueprint.NewProvider(ProguardSpecInfo{})

// JavaInfo contains information about a java module for use by modules that depend on it.
type JavaInfo struct {
	// HeaderJars is a list of jars that can be passed as the javac classpath in order to link
	// against this module.  If empty, ImplementationJars should be used instead.
	HeaderJars android.Paths

	// set of header jars for all transitive libs deps
	TransitiveLibsHeaderJars *android.DepSet[android.Path]

	// set of header jars for all transitive static libs deps
	TransitiveStaticLibsHeaderJars *android.DepSet[android.Path]

	// ImplementationAndResourceJars is a list of jars that contain the implementations of classes
	// in the module as well as any resources included in the module.
	ImplementationAndResourcesJars android.Paths

	// ImplementationJars is a list of jars that contain the implementations of classes in the
	//module.
	ImplementationJars android.Paths

	// ResourceJars is a list of jars that contain the resources included in the module.
	ResourceJars android.Paths

	// AidlIncludeDirs is a list of directories that should be passed to the aidl tool when
	// depending on this module.
	AidlIncludeDirs android.Paths

	// SrcJarArgs is a list of arguments to pass to soong_zip to package the sources of this
	// module.
	SrcJarArgs []string

	// SrcJarDeps is a list of paths to depend on when packaging the sources of this module.
	SrcJarDeps android.Paths

	// ExportedPlugins is a list of paths that should be used as annotation processors for any
	// module that depends on this module.
	ExportedPlugins android.Paths

	// ExportedPluginClasses is a list of classes that should be run as annotation processors for
	// any module that depends on this module.
	ExportedPluginClasses []string

	// ExportedPluginDisableTurbine is true if this module's annotation processors generate APIs,
	// requiring disbling turbine for any modules that depend on it.
	ExportedPluginDisableTurbine bool

	// JacocoReportClassesFile is the path to a jar containing uninstrumented classes that will be
	// instrumented by jacoco.
	JacocoReportClassesFile android.Path

	// set of aconfig flags for all transitive libs deps
	// TODO(joeo): It would be nice if this were over in the aconfig package instead of here.
	// In order to do that, generated_java_library would need a way doing
	// collectTransitiveAconfigFiles with one of the callbacks, and having that automatically
	// propagated. If we were to clean up more of the stuff on JavaInfo that's not part of
	// core java rules (e.g. AidlIncludeDirs), then maybe adding more framework to do that would be
	// worth it.
	TransitiveAconfigFiles *android.DepSet[android.Path]
}

var JavaInfoProvider = blueprint.NewProvider(JavaInfo{})

// SyspropPublicStubInfo contains info about the sysprop public stub library that corresponds to
// the sysprop implementation library.
type SyspropPublicStubInfo struct {
	// JavaInfo is the JavaInfoProvider of the sysprop public stub library that corresponds to
	// the sysprop implementation library.
	JavaInfo JavaInfo
}

var SyspropPublicStubInfoProvider = blueprint.NewProvider(SyspropPublicStubInfo{})

// Methods that need to be implemented for a module that is added to apex java_libs property.
type ApexDependency interface {
	HeaderJars() android.Paths
	ImplementationAndResourcesJars() android.Paths
}

// Provides build path and install path to DEX jars.
type UsesLibraryDependency interface {
	DexJarBuildPath() OptionalDexJarPath
	DexJarInstallPath() android.Path
	ClassLoaderContexts() dexpreopt.ClassLoaderContextMap
}

// TODO(jungjw): Move this to kythe.go once it's created.
type xref interface {
	XrefJavaFiles() android.Paths
}

func (j *Module) XrefJavaFiles() android.Paths {
	return j.kytheFiles
}

type dependencyTag struct {
	blueprint.BaseDependencyTag
	name string

	// True if the dependency is relinked at runtime.
	runtimeLinked bool

	// True if the dependency is a toolchain, for example an annotation processor.
	toolchain bool
}

// installDependencyTag is a dependency tag that is annotated to cause the installed files of the
// dependency to be installed when the parent module is installed.
type installDependencyTag struct {
	blueprint.BaseDependencyTag
	android.InstallAlwaysNeededDependencyTag
	name string
}

func (d dependencyTag) LicenseAnnotations() []android.LicenseAnnotation {
	if d.runtimeLinked {
		return []android.LicenseAnnotation{android.LicenseAnnotationSharedDependency}
	} else if d.toolchain {
		return []android.LicenseAnnotation{android.LicenseAnnotationToolchain}
	}
	return nil
}

var _ android.LicenseAnnotationsDependencyTag = dependencyTag{}

type usesLibraryDependencyTag struct {
	dependencyTag
	sdkVersion int  // SDK version in which the library appared as a standalone library.
	optional   bool // If the dependency is optional or required.
}

func makeUsesLibraryDependencyTag(sdkVersion int, optional bool) usesLibraryDependencyTag {
	return usesLibraryDependencyTag{
		dependencyTag: dependencyTag{
			name:          fmt.Sprintf("uses-library-%d", sdkVersion),
			runtimeLinked: true,
		},
		sdkVersion: sdkVersion,
		optional:   optional,
	}
}

func IsJniDepTag(depTag blueprint.DependencyTag) bool {
	return depTag == jniLibTag
}

var (
	dataNativeBinsTag       = dependencyTag{name: "dataNativeBins"}
	dataDeviceBinsTag       = dependencyTag{name: "dataDeviceBins"}
	staticLibTag            = dependencyTag{name: "staticlib"}
	libTag                  = dependencyTag{name: "javalib", runtimeLinked: true}
	sdkLibTag               = dependencyTag{name: "sdklib", runtimeLinked: true}
	java9LibTag             = dependencyTag{name: "java9lib", runtimeLinked: true}
	pluginTag               = dependencyTag{name: "plugin", toolchain: true}
	errorpronePluginTag     = dependencyTag{name: "errorprone-plugin", toolchain: true}
	exportedPluginTag       = dependencyTag{name: "exported-plugin", toolchain: true}
	bootClasspathTag        = dependencyTag{name: "bootclasspath", runtimeLinked: true}
	systemModulesTag        = dependencyTag{name: "system modules", runtimeLinked: true}
	frameworkResTag         = dependencyTag{name: "framework-res"}
	kotlinStdlibTag         = dependencyTag{name: "kotlin-stdlib", runtimeLinked: true}
	kotlinAnnotationsTag    = dependencyTag{name: "kotlin-annotations", runtimeLinked: true}
	kotlinPluginTag         = dependencyTag{name: "kotlin-plugin", toolchain: true}
	proguardRaiseTag        = dependencyTag{name: "proguard-raise"}
	certificateTag          = dependencyTag{name: "certificate"}
	instrumentationForTag   = dependencyTag{name: "instrumentation_for"}
	extraLintCheckTag       = dependencyTag{name: "extra-lint-check", toolchain: true}
	jniLibTag               = dependencyTag{name: "jnilib", runtimeLinked: true}
	r8LibraryJarTag         = dependencyTag{name: "r8-libraryjar", runtimeLinked: true}
	syspropPublicStubDepTag = dependencyTag{name: "sysprop public stub"}
	javaApiContributionTag  = dependencyTag{name: "java-api-contribution"}
	depApiSrcsTag           = dependencyTag{name: "dep-api-srcs"}
	jniInstallTag           = installDependencyTag{name: "jni install"}
	binaryInstallTag        = installDependencyTag{name: "binary install"}
	usesLibReqTag           = makeUsesLibraryDependencyTag(dexpreopt.AnySdkVersion, false)
	usesLibOptTag           = makeUsesLibraryDependencyTag(dexpreopt.AnySdkVersion, true)
	usesLibCompat28OptTag   = makeUsesLibraryDependencyTag(28, true)
	usesLibCompat29ReqTag   = makeUsesLibraryDependencyTag(29, false)
	usesLibCompat30OptTag   = makeUsesLibraryDependencyTag(30, true)
)

func IsLibDepTag(depTag blueprint.DependencyTag) bool {
	return depTag == libTag || depTag == sdkLibTag
}

func IsStaticLibDepTag(depTag blueprint.DependencyTag) bool {
	return depTag == staticLibTag
}

type sdkDep struct {
	useModule, useFiles, invalidVersion bool

	// The modules that will be added to the bootclasspath when targeting 1.8 or lower
	bootclasspath []string

	// The default system modules to use. Will be an empty string if no system
	// modules are to be used.
	systemModules string

	// The modules that will be added to the classpath regardless of the Java language level targeted
	classpath []string

	// The modules that will be added ot the classpath when targeting 1.9 or higher
	// (normally these will be on the bootclasspath when targeting 1.8 or lower)
	java9Classpath []string

	frameworkResModule string

	jars android.Paths
	aidl android.OptionalPath

	noStandardLibs, noFrameworksLibs bool
}

func (s sdkDep) hasStandardLibs() bool {
	return !s.noStandardLibs
}

func (s sdkDep) hasFrameworkLibs() bool {
	return !s.noStandardLibs && !s.noFrameworksLibs
}

type jniLib struct {
	name           string
	path           android.Path
	target         android.Target
	coverageFile   android.OptionalPath
	unstrippedFile android.Path
	partition      string
}

func sdkDeps(ctx android.BottomUpMutatorContext, sdkContext android.SdkContext, d dexer) {
	sdkDep := decodeSdkDep(ctx, sdkContext)
	if sdkDep.useModule {
		ctx.AddVariationDependencies(nil, bootClasspathTag, sdkDep.bootclasspath...)
		ctx.AddVariationDependencies(nil, java9LibTag, sdkDep.java9Classpath...)
		ctx.AddVariationDependencies(nil, sdkLibTag, sdkDep.classpath...)
		if d.effectiveOptimizeEnabled() && sdkDep.hasStandardLibs() {
			ctx.AddVariationDependencies(nil, proguardRaiseTag,
				config.LegacyCorePlatformBootclasspathLibraries...,
			)
		}
		if d.effectiveOptimizeEnabled() && sdkDep.hasFrameworkLibs() {
			ctx.AddVariationDependencies(nil, proguardRaiseTag, config.FrameworkLibraries...)
		}
	}
	if sdkDep.systemModules != "" {
		ctx.AddVariationDependencies(nil, systemModulesTag, sdkDep.systemModules)
	}
}

type deps struct {
	// bootClasspath is the list of jars that form the boot classpath (generally the java.* and
	// android.* classes) for tools that still use it.  javac targeting 1.9 or higher uses
	// systemModules and java9Classpath instead.
	bootClasspath classpath

	// classpath is the list of jars that form the classpath for javac and kotlinc rules.  It
	// contains header jars for all static and non-static dependencies.
	classpath classpath

	// dexClasspath is the list of jars that form the classpath for d8 and r8 rules.  It contains
	// header jars for all non-static dependencies.  Static dependencies have already been
	// combined into the program jar.
	dexClasspath classpath

	// java9Classpath is the list of jars that will be added to the classpath when targeting
	// 1.9 or higher.  It generally contains the android.* classes, while the java.* classes
	// are provided by systemModules.
	java9Classpath classpath

	processorPath           classpath
	errorProneProcessorPath classpath
	processorClasses        []string
	staticJars              android.Paths
	staticHeaderJars        android.Paths
	staticResourceJars      android.Paths
	aidlIncludeDirs         android.Paths
	srcs                    android.Paths
	srcJars                 android.Paths
	systemModules           *systemModules
	aidlPreprocess          android.OptionalPath
	kotlinStdlib            android.Paths
	kotlinAnnotations       android.Paths
	kotlinPlugins           android.Paths

	disableTurbine bool
}

func checkProducesJars(ctx android.ModuleContext, dep android.SourceFileProducer) {
	for _, f := range dep.Srcs() {
		if f.Ext() != ".jar" {
			ctx.ModuleErrorf("genrule %q must generate files ending with .jar to be used as a libs or static_libs dependency",
				ctx.OtherModuleName(dep.(blueprint.Module)))
		}
	}
}

func getJavaVersion(ctx android.ModuleContext, javaVersion string, sdkContext android.SdkContext) javaVersion {
	if javaVersion != "" {
		return normalizeJavaVersion(ctx, javaVersion)
	} else if ctx.Device() {
		return defaultJavaLanguageVersion(ctx, sdkContext.SdkVersion(ctx))
	} else {
		return JAVA_VERSION_17
	}
}

// Java version for stubs generation
func getStubsJavaVersion() javaVersion {
	return JAVA_VERSION_8
}

type javaVersion int

const (
	JAVA_VERSION_UNSUPPORTED = 0
	JAVA_VERSION_6           = 6
	JAVA_VERSION_7           = 7
	JAVA_VERSION_8           = 8
	JAVA_VERSION_9           = 9
	JAVA_VERSION_11          = 11
	JAVA_VERSION_17          = 17
)

func (v javaVersion) String() string {
	switch v {
	case JAVA_VERSION_6:
		return "1.6"
	case JAVA_VERSION_7:
		return "1.7"
	case JAVA_VERSION_8:
		return "1.8"
	case JAVA_VERSION_9:
		return "1.9"
	case JAVA_VERSION_11:
		return "11"
	case JAVA_VERSION_17:
		return "17"
	default:
		return "unsupported"
	}
}

func (v javaVersion) StringForKotlinc() string {
	// $ ./external/kotlinc/bin/kotlinc -jvm-target foo
	// error: unknown JVM target version: foo
	// Supported versions: 1.6, 1.8, 9, 10, 11, 12, 13, 14, 15, 16, 17
	switch v {
	case JAVA_VERSION_7:
		return "1.6"
	case JAVA_VERSION_9:
		return "9"
	default:
		return v.String()
	}
}

// Returns true if javac targeting this version uses system modules instead of a bootclasspath.
func (v javaVersion) usesJavaModules() bool {
	return v >= 9
}

func normalizeJavaVersion(ctx android.BaseModuleContext, javaVersion string) javaVersion {
	switch javaVersion {
	case "1.6", "6":
		return JAVA_VERSION_6
	case "1.7", "7":
		return JAVA_VERSION_7
	case "1.8", "8":
		return JAVA_VERSION_8
	case "1.9", "9":
		return JAVA_VERSION_9
	case "11":
		return JAVA_VERSION_11
	case "17":
		return JAVA_VERSION_17
	case "10", "12", "13", "14", "15", "16":
		ctx.PropertyErrorf("java_version", "Java language level %s is not supported", javaVersion)
		return JAVA_VERSION_UNSUPPORTED
	default:
		ctx.PropertyErrorf("java_version", "Unrecognized Java language level")
		return JAVA_VERSION_UNSUPPORTED
	}
}

//
// Java libraries (.jar file)
//

type Library struct {
	Module

	exportedProguardFlagFiles android.Paths

	InstallMixin func(ctx android.ModuleContext, installPath android.Path) (extraInstallDeps android.Paths)
}

var _ android.ApexModule = (*Library)(nil)

// Provides access to the list of permitted packages from apex boot jars.
type PermittedPackagesForUpdatableBootJars interface {
	PermittedPackagesForUpdatableBootJars() []string
}

var _ PermittedPackagesForUpdatableBootJars = (*Library)(nil)

func (j *Library) PermittedPackagesForUpdatableBootJars() []string {
	return j.properties.Permitted_packages
}

func shouldUncompressDex(ctx android.ModuleContext, dexpreopter *dexpreopter) bool {
	// Store uncompressed (and aligned) any dex files from jars in APEXes.
	if apexInfo := ctx.Provider(android.ApexInfoProvider).(android.ApexInfo); !apexInfo.IsForPlatform() {
		return true
	}

	// Store uncompressed (and do not strip) dex files from boot class path jars.
	if inList(ctx.ModuleName(), ctx.Config().BootJars()) {
		return true
	}

	// Store uncompressed dex files that are preopted on /system.
	if !dexpreopter.dexpreoptDisabled(ctx) && (ctx.Host() || !dexpreopter.odexOnSystemOther(ctx, dexpreopter.installPath)) {
		return true
	}
	if ctx.Config().UncompressPrivAppDex() &&
		inList(ctx.ModuleName(), ctx.Config().ModulesLoadedByPrivilegedModules()) {
		return true
	}

	return false
}

// Sets `dexer.dexProperties.Uncompress_dex` to the proper value.
func setUncompressDex(ctx android.ModuleContext, dexpreopter *dexpreopter, dexer *dexer) {
	if dexer.dexProperties.Uncompress_dex == nil {
		// If the value was not force-set by the user, use reasonable default based on the module.
		dexer.dexProperties.Uncompress_dex = proptools.BoolPtr(shouldUncompressDex(ctx, dexpreopter))
	}
}

func (j *Library) GenerateAndroidBuildActions(ctx android.ModuleContext) {

	j.provideHiddenAPIPropertyInfo(ctx)

	j.sdkVersion = j.SdkVersion(ctx)
	j.minSdkVersion = j.MinSdkVersion(ctx)
	j.maxSdkVersion = j.MaxSdkVersion(ctx)

	j.stem = proptools.StringDefault(j.overridableDeviceProperties.Stem, ctx.ModuleName())

	apexInfo := ctx.Provider(android.ApexInfoProvider).(android.ApexInfo)
	if !apexInfo.IsForPlatform() {
		j.hideApexVariantFromMake = true
	}

	j.checkSdkVersions(ctx)
	j.checkHeadersOnly(ctx)
	if ctx.Device() {
		j.dexpreopter.installPath = j.dexpreopter.getInstallPath(
			ctx, android.PathForModuleInstall(ctx, "framework", j.Stem()+".jar"))
		j.dexpreopter.isSDKLibrary = j.deviceProperties.IsSDKLibrary
		setUncompressDex(ctx, &j.dexpreopter, &j.dexer)
		j.dexpreopter.uncompressedDex = *j.dexProperties.Uncompress_dex
		j.classLoaderContexts = j.usesLibrary.classLoaderContextForUsesLibDeps(ctx)
	}
	j.compile(ctx, nil, nil, nil)

	// Collect the module directory for IDE info in java/jdeps.go.
	j.modulePaths = append(j.modulePaths, ctx.ModuleDir())

	exclusivelyForApex := !apexInfo.IsForPlatform()
	if (Bool(j.properties.Installable) || ctx.Host()) && !exclusivelyForApex {
		var extraInstallDeps android.Paths
		if j.InstallMixin != nil {
			extraInstallDeps = j.InstallMixin(ctx, j.outputFile)
		}
		hostDexNeeded := Bool(j.deviceProperties.Hostdex) && !ctx.Host()
		if hostDexNeeded {
			j.hostdexInstallFile = ctx.InstallFile(
				android.PathForHostDexInstall(ctx, "framework"),
				j.Stem()+"-hostdex.jar", j.outputFile)
		}
		var installDir android.InstallPath
		if ctx.InstallInTestcases() {
			var archDir string
			if !ctx.Host() {
				archDir = ctx.DeviceConfig().DeviceArch()
			}
			installDir = android.PathForModuleInstall(ctx, ctx.ModuleName(), archDir)
		} else {
			installDir = android.PathForModuleInstall(ctx, "framework")
		}
		j.installFile = ctx.InstallFile(installDir, j.Stem()+".jar", j.outputFile, extraInstallDeps...)
	}

	proguardSpecInfo := j.collectProguardSpecInfo(ctx)
	ctx.SetProvider(ProguardSpecInfoProvider, proguardSpecInfo)
	j.exportedProguardFlagFiles = proguardSpecInfo.ProguardFlagsFiles.ToList()
}

func (j *Library) DepsMutator(ctx android.BottomUpMutatorContext) {
	j.deps(ctx)
	j.usesLibrary.deps(ctx, false)
}

const (
	aidlIncludeDir   = "aidl"
	javaDir          = "java"
	jarFileSuffix    = ".jar"
	testConfigSuffix = "-AndroidTest.xml"
)

// path to the jar file of a java library. Relative to <sdk_root>/<api_dir>
func sdkSnapshotFilePathForJar(_ android.SdkMemberContext, osPrefix, name string) string {
	return sdkSnapshotFilePathForMember(osPrefix, name, jarFileSuffix)
}

func sdkSnapshotFilePathForMember(osPrefix, name string, suffix string) string {
	return filepath.Join(javaDir, osPrefix, name+suffix)
}

type librarySdkMemberType struct {
	android.SdkMemberTypeBase

	// Function to retrieve the appropriate output jar (implementation or header) from
	// the library.
	jarToExportGetter func(ctx android.SdkMemberContext, j *Library) android.Path

	// Function to compute the snapshot relative path to which the named library's
	// jar should be copied.
	snapshotPathGetter func(ctx android.SdkMemberContext, osPrefix, name string) string

	// True if only the jar should be copied to the snapshot, false if the jar plus any additional
	// files like aidl files should also be copied.
	onlyCopyJarToSnapshot bool
}

const (
	onlyCopyJarToSnapshot    = true
	copyEverythingToSnapshot = false
)

func (mt *librarySdkMemberType) AddDependencies(ctx android.SdkDependencyContext, dependencyTag blueprint.DependencyTag, names []string) {
	ctx.AddVariationDependencies(nil, dependencyTag, names...)
}

func (mt *librarySdkMemberType) IsInstance(module android.Module) bool {
	_, ok := module.(*Library)
	return ok
}

func (mt *librarySdkMemberType) AddPrebuiltModule(ctx android.SdkMemberContext, member android.SdkMember) android.BpModule {
	return ctx.SnapshotBuilder().AddPrebuiltModule(member, "java_import")
}

func (mt *librarySdkMemberType) CreateVariantPropertiesStruct() android.SdkMemberProperties {
	return &librarySdkMemberProperties{}
}

type librarySdkMemberProperties struct {
	android.SdkMemberPropertiesBase

	JarToExport     android.Path `android:"arch_variant"`
	AidlIncludeDirs android.Paths

	// The list of permitted packages that need to be passed to the prebuilts as they are used to
	// create the updatable-bcp-packages.txt file.
	PermittedPackages []string

	// The value of the min_sdk_version property, translated into a number where possible.
	MinSdkVersion *string `supported_build_releases:"Tiramisu+"`

	DexPreoptProfileGuided *bool `supported_build_releases:"UpsideDownCake+"`
}

func (p *librarySdkMemberProperties) PopulateFromVariant(ctx android.SdkMemberContext, variant android.Module) {
	j := variant.(*Library)

	p.JarToExport = ctx.MemberType().(*librarySdkMemberType).jarToExportGetter(ctx, j)

	p.AidlIncludeDirs = j.AidlIncludeDirs()

	p.PermittedPackages = j.PermittedPackagesForUpdatableBootJars()

	// If the min_sdk_version was set then add the canonical representation of the API level to the
	// snapshot.
	if j.deviceProperties.Min_sdk_version != nil {
		canonical, err := android.ReplaceFinalizedCodenames(ctx.SdkModuleContext().Config(), j.minSdkVersion.String())
		if err != nil {
			ctx.ModuleErrorf("%s", err)
		}
		p.MinSdkVersion = proptools.StringPtr(canonical)
	}

	if j.dexpreopter.dexpreoptProperties.Dex_preopt_result.Profile_guided {
		p.DexPreoptProfileGuided = proptools.BoolPtr(true)
	}
}

func (p *librarySdkMemberProperties) AddToPropertySet(ctx android.SdkMemberContext, propertySet android.BpPropertySet) {
	builder := ctx.SnapshotBuilder()

	memberType := ctx.MemberType().(*librarySdkMemberType)

	exportedJar := p.JarToExport
	if exportedJar != nil {
		// Delegate the creation of the snapshot relative path to the member type.
		snapshotRelativeJavaLibPath := memberType.snapshotPathGetter(ctx, p.OsPrefix(), ctx.Name())

		// Copy the exported jar to the snapshot.
		builder.CopyToSnapshot(exportedJar, snapshotRelativeJavaLibPath)

		propertySet.AddProperty("jars", []string{snapshotRelativeJavaLibPath})
	}

	if p.MinSdkVersion != nil {
		propertySet.AddProperty("min_sdk_version", *p.MinSdkVersion)
	}

	if len(p.PermittedPackages) > 0 {
		propertySet.AddProperty("permitted_packages", p.PermittedPackages)
	}

	dexPreoptSet := propertySet.AddPropertySet("dex_preopt")
	if p.DexPreoptProfileGuided != nil {
		dexPreoptSet.AddProperty("profile_guided", proptools.Bool(p.DexPreoptProfileGuided))
	}

	// Do not copy anything else to the snapshot.
	if memberType.onlyCopyJarToSnapshot {
		return
	}

	aidlIncludeDirs := p.AidlIncludeDirs
	if len(aidlIncludeDirs) != 0 {
		sdkModuleContext := ctx.SdkModuleContext()
		for _, dir := range aidlIncludeDirs {
			// TODO(jiyong): copy parcelable declarations only
			aidlFiles, _ := sdkModuleContext.GlobWithDeps(dir.String()+"/**/*.aidl", nil)
			for _, file := range aidlFiles {
				builder.CopyToSnapshot(android.PathForSource(sdkModuleContext, file), filepath.Join(aidlIncludeDir, file))
			}
		}

		// TODO(b/151933053) - add aidl include dirs property
	}
}

// java_library builds and links sources into a `.jar` file for the device, and possibly for the host as well.
//
// By default, a java_library has a single variant that produces a `.jar` file containing `.class` files that were
// compiled against the device bootclasspath.  This jar is not suitable for installing on a device, but can be used
// as a `static_libs` dependency of another module.
//
// Specifying `installable: true` will product a `.jar` file containing `classes.dex` files, suitable for installing on
// a device.
//
// Specifying `host_supported: true` will produce two variants, one compiled against the device bootclasspath and one
// compiled against the host bootclasspath.
func LibraryFactory() android.Module {
	module := &Library{}

	module.addHostAndDeviceProperties()

	module.initModuleAndImport(module)

	android.InitApexModule(module)
	android.InitBazelModule(module)
	InitJavaModule(module, android.HostAndDeviceSupported)
	return module
}

// java_library_static is an obsolete alias for java_library.
func LibraryStaticFactory() android.Module {
	return LibraryFactory()
}

// java_library_host builds and links sources into a `.jar` file for the host.
//
// A java_library_host has a single variant that produces a `.jar` file containing `.class` files that were
// compiled against the host bootclasspath.
func LibraryHostFactory() android.Module {
	module := &Library{}

	module.addHostProperties()

	module.Module.properties.Installable = proptools.BoolPtr(true)

	android.InitApexModule(module)
	android.InitBazelModule(module)
	InitJavaModule(module, android.HostSupported)
	return module
}

//
// Java Tests
//

// Test option struct.
type TestOptions struct {
	android.CommonTestOptions

	// a list of extra test configuration files that should be installed with the module.
	Extra_test_configs []string `android:"path,arch_variant"`

	// Extra <option> tags to add to the auto generated test xml file. The "key"
	// is optional in each of these.
	Tradefed_options []tradefed.Option

	// Extra <option> tags to add to the auto generated test xml file under the test runner, e.g., AndroidJunitTest.
	// The "key" is optional in each of these.
	Test_runner_options []tradefed.Option
}

type testProperties struct {
	// list of compatibility suites (for example "cts", "vts") that the module should be
	// installed into.
	Test_suites []string `android:"arch_variant"`

	// the name of the test configuration (for example "AndroidTest.xml") that should be
	// installed with the module.
	Test_config *string `android:"path,arch_variant"`

	// the name of the test configuration template (for example "AndroidTestTemplate.xml") that
	// should be installed with the module.
	Test_config_template *string `android:"path,arch_variant"`

	// list of files or filegroup modules that provide data that should be installed alongside
	// the test
	Data []string `android:"path"`

	// Flag to indicate whether or not to create test config automatically. If AndroidTest.xml
	// doesn't exist next to the Android.bp, this attribute doesn't need to be set to true
	// explicitly.
	Auto_gen_config *bool

	// Add parameterized mainline modules to auto generated test config. The options will be
	// handled by TradeFed to do downloading and installing the specified modules on the device.
	Test_mainline_modules []string

	// Test options.
	Test_options TestOptions

	// Names of modules containing JNI libraries that should be installed alongside the test.
	Jni_libs []string

	// Install the test into a folder named for the module in all test suites.
	Per_testcase_directory *bool
}

type hostTestProperties struct {
	// list of native binary modules that should be installed alongside the test
	Data_native_bins []string `android:"arch_variant"`

	// list of device binary modules that should be installed alongside the test
	// This property only adds the first variant of the dependency
	Data_device_bins_first []string `android:"arch_variant"`

	// list of device binary modules that should be installed alongside the test
	// This property adds 64bit AND 32bit variants of the dependency
	Data_device_bins_both []string `android:"arch_variant"`

	// list of device binary modules that should be installed alongside the test
	// This property only adds 64bit variants of the dependency
	Data_device_bins_64 []string `android:"arch_variant"`

	// list of device binary modules that should be installed alongside the test
	// This property adds 32bit variants of the dependency if available, or else
	// defaults to the 64bit variant
	Data_device_bins_prefer32 []string `android:"arch_variant"`

	// list of device binary modules that should be installed alongside the test
	// This property only adds 32bit variants of the dependency
	Data_device_bins_32 []string `android:"arch_variant"`
}

type testHelperLibraryProperties struct {
	// list of compatibility suites (for example "cts", "vts") that the module should be
	// installed into.
	Test_suites []string `android:"arch_variant"`

	// Install the test into a folder named for the module in all test suites.
	Per_testcase_directory *bool
}

type prebuiltTestProperties struct {
	// list of compatibility suites (for example "cts", "vts") that the module should be
	// installed into.
	Test_suites []string `android:"arch_variant"`

	// the name of the test configuration (for example "AndroidTest.xml") that should be
	// installed with the module.
	Test_config *string `android:"path,arch_variant"`
}

type Test struct {
	Library

	testProperties testProperties

	testConfig       android.Path
	extraTestConfigs android.Paths
	data             android.Paths
}

type TestHost struct {
	Test

	testHostProperties hostTestProperties
}

type TestHelperLibrary struct {
	Library

	testHelperLibraryProperties testHelperLibraryProperties
}

type JavaTestImport struct {
	Import

	prebuiltTestProperties prebuiltTestProperties

	testConfig android.Path
	dexJarFile android.Path
}

func (j *Test) InstallInTestcases() bool {
	// Host java tests install into $(HOST_OUT_JAVA_LIBRARIES), and then are copied into
	// testcases by base_rules.mk.
	return !j.Host()
}

func (j *TestHelperLibrary) InstallInTestcases() bool {
	return true
}

func (j *JavaTestImport) InstallInTestcases() bool {
	return true
}

func (j *TestHost) addDataDeviceBinsDeps(ctx android.BottomUpMutatorContext) {
	if len(j.testHostProperties.Data_device_bins_first) > 0 {
		deviceVariations := ctx.Config().AndroidFirstDeviceTarget.Variations()
		ctx.AddFarVariationDependencies(deviceVariations, dataDeviceBinsTag, j.testHostProperties.Data_device_bins_first...)
	}

	var maybeAndroid32Target *android.Target
	var maybeAndroid64Target *android.Target
	android32TargetList := android.FirstTarget(ctx.Config().Targets[android.Android], "lib32")
	android64TargetList := android.FirstTarget(ctx.Config().Targets[android.Android], "lib64")
	if len(android32TargetList) > 0 {
		maybeAndroid32Target = &android32TargetList[0]
	}
	if len(android64TargetList) > 0 {
		maybeAndroid64Target = &android64TargetList[0]
	}

	if len(j.testHostProperties.Data_device_bins_both) > 0 {
		if maybeAndroid32Target == nil && maybeAndroid64Target == nil {
			ctx.PropertyErrorf("data_device_bins_both", "no device targets available. Targets: %q", ctx.Config().Targets)
			return
		}
		if maybeAndroid32Target != nil {
			ctx.AddFarVariationDependencies(
				maybeAndroid32Target.Variations(),
				dataDeviceBinsTag,
				j.testHostProperties.Data_device_bins_both...,
			)
		}
		if maybeAndroid64Target != nil {
			ctx.AddFarVariationDependencies(
				maybeAndroid64Target.Variations(),
				dataDeviceBinsTag,
				j.testHostProperties.Data_device_bins_both...,
			)
		}
	}

	if len(j.testHostProperties.Data_device_bins_prefer32) > 0 {
		if maybeAndroid32Target != nil {
			ctx.AddFarVariationDependencies(
				maybeAndroid32Target.Variations(),
				dataDeviceBinsTag,
				j.testHostProperties.Data_device_bins_prefer32...,
			)
		} else {
			if maybeAndroid64Target == nil {
				ctx.PropertyErrorf("data_device_bins_prefer32", "no device targets available. Targets: %q", ctx.Config().Targets)
				return
			}
			ctx.AddFarVariationDependencies(
				maybeAndroid64Target.Variations(),
				dataDeviceBinsTag,
				j.testHostProperties.Data_device_bins_prefer32...,
			)
		}
	}

	if len(j.testHostProperties.Data_device_bins_32) > 0 {
		if maybeAndroid32Target == nil {
			ctx.PropertyErrorf("data_device_bins_32", "cannot find 32bit device target. Targets: %q", ctx.Config().Targets)
			return
		}
		deviceVariations := maybeAndroid32Target.Variations()
		ctx.AddFarVariationDependencies(deviceVariations, dataDeviceBinsTag, j.testHostProperties.Data_device_bins_32...)
	}

	if len(j.testHostProperties.Data_device_bins_64) > 0 {
		if maybeAndroid64Target == nil {
			ctx.PropertyErrorf("data_device_bins_64", "cannot find 64bit device target. Targets: %q", ctx.Config().Targets)
			return
		}
		deviceVariations := maybeAndroid64Target.Variations()
		ctx.AddFarVariationDependencies(deviceVariations, dataDeviceBinsTag, j.testHostProperties.Data_device_bins_64...)
	}
}

func (j *TestHost) DepsMutator(ctx android.BottomUpMutatorContext) {
	if len(j.testHostProperties.Data_native_bins) > 0 {
		for _, target := range ctx.MultiTargets() {
			ctx.AddVariationDependencies(target.Variations(), dataNativeBinsTag, j.testHostProperties.Data_native_bins...)
		}
	}

	if len(j.testProperties.Jni_libs) > 0 {
		for _, target := range ctx.MultiTargets() {
			sharedLibVariations := append(target.Variations(), blueprint.Variation{Mutator: "link", Variation: "shared"})
			ctx.AddFarVariationDependencies(sharedLibVariations, jniLibTag, j.testProperties.Jni_libs...)
		}
	}

	j.addDataDeviceBinsDeps(ctx)
	j.deps(ctx)
}

func (j *TestHost) AddExtraResource(p android.Path) {
	j.extraResources = append(j.extraResources, p)
}

func (j *TestHost) dataDeviceBins() []string {
	ret := make([]string, 0,
		len(j.testHostProperties.Data_device_bins_first)+
			len(j.testHostProperties.Data_device_bins_both)+
			len(j.testHostProperties.Data_device_bins_prefer32)+
			len(j.testHostProperties.Data_device_bins_32)+
			len(j.testHostProperties.Data_device_bins_64),
	)

	ret = append(ret, j.testHostProperties.Data_device_bins_first...)
	ret = append(ret, j.testHostProperties.Data_device_bins_both...)
	ret = append(ret, j.testHostProperties.Data_device_bins_prefer32...)
	ret = append(ret, j.testHostProperties.Data_device_bins_32...)
	ret = append(ret, j.testHostProperties.Data_device_bins_64...)

	return ret
}

func (j *TestHost) GenerateAndroidBuildActions(ctx android.ModuleContext) {
	var configs []tradefed.Config
	dataDeviceBins := j.dataDeviceBins()
	if len(dataDeviceBins) > 0 {
		// add Tradefed configuration to push device bins to device for testing
		remoteDir := filepath.Join("/data/local/tests/unrestricted/", j.Name())
		options := []tradefed.Option{{Name: "cleanup", Value: "true"}}
		for _, bin := range dataDeviceBins {
			fullPath := filepath.Join(remoteDir, bin)
			options = append(options, tradefed.Option{Name: "push-file", Key: bin, Value: fullPath})
		}
		configs = append(configs, tradefed.Object{
			Type:    "target_preparer",
			Class:   "com.android.tradefed.targetprep.PushFilePreparer",
			Options: options,
		})
	}

	j.Test.generateAndroidBuildActionsWithConfig(ctx, configs)
}

func (j *Test) GenerateAndroidBuildActions(ctx android.ModuleContext) {
	j.generateAndroidBuildActionsWithConfig(ctx, nil)
}

func (j *Test) generateAndroidBuildActionsWithConfig(ctx android.ModuleContext, configs []tradefed.Config) {
	if j.testProperties.Test_options.Unit_test == nil && ctx.Host() {
		// TODO(b/): Clean temporary heuristic to avoid unexpected onboarding.
		defaultUnitTest := !inList("tradefed", j.properties.Libs) && !inList("cts", j.testProperties.Test_suites)
		j.testProperties.Test_options.Unit_test = proptools.BoolPtr(defaultUnitTest)
	}
	j.testConfig = tradefed.AutoGenTestConfig(ctx, tradefed.AutoGenTestConfigOptions{
		TestConfigProp:          j.testProperties.Test_config,
		TestConfigTemplateProp:  j.testProperties.Test_config_template,
		TestSuites:              j.testProperties.Test_suites,
		Config:                  configs,
		OptionsForAutogenerated: j.testProperties.Test_options.Tradefed_options,
		TestRunnerOptions:       j.testProperties.Test_options.Test_runner_options,
		AutoGenConfig:           j.testProperties.Auto_gen_config,
		UnitTest:                j.testProperties.Test_options.Unit_test,
		DeviceTemplate:          "${JavaTestConfigTemplate}",
		HostTemplate:            "${JavaHostTestConfigTemplate}",
		HostUnitTestTemplate:    "${JavaHostUnitTestConfigTemplate}",
	})

	j.data = android.PathsForModuleSrc(ctx, j.testProperties.Data)

	j.extraTestConfigs = android.PathsForModuleSrc(ctx, j.testProperties.Test_options.Extra_test_configs)

	ctx.VisitDirectDepsWithTag(dataNativeBinsTag, func(dep android.Module) {
		j.data = append(j.data, android.OutputFileForModule(ctx, dep, ""))
	})

	ctx.VisitDirectDepsWithTag(dataDeviceBinsTag, func(dep android.Module) {
		j.data = append(j.data, android.OutputFileForModule(ctx, dep, ""))
	})

	ctx.VisitDirectDepsWithTag(jniLibTag, func(dep android.Module) {
		sharedLibInfo := ctx.OtherModuleProvider(dep, cc.SharedLibraryInfoProvider).(cc.SharedLibraryInfo)
		if sharedLibInfo.SharedLibrary != nil {
			// Copy to an intermediate output directory to append "lib[64]" to the path,
			// so that it's compatible with the default rpath values.
			var relPath string
			if sharedLibInfo.Target.Arch.ArchType.Multilib == "lib64" {
				relPath = filepath.Join("lib64", sharedLibInfo.SharedLibrary.Base())
			} else {
				relPath = filepath.Join("lib", sharedLibInfo.SharedLibrary.Base())
			}
			relocatedLib := android.PathForModuleOut(ctx, "relocated").Join(ctx, relPath)
			ctx.Build(pctx, android.BuildParams{
				Rule:   android.Cp,
				Input:  sharedLibInfo.SharedLibrary,
				Output: relocatedLib,
			})
			j.data = append(j.data, relocatedLib)
		} else {
			ctx.PropertyErrorf("jni_libs", "%q of type %q is not supported", dep.Name(), ctx.OtherModuleType(dep))
		}
	})

	j.Library.GenerateAndroidBuildActions(ctx)
}

func (j *TestHelperLibrary) GenerateAndroidBuildActions(ctx android.ModuleContext) {
	j.Library.GenerateAndroidBuildActions(ctx)
}

func (j *JavaTestImport) GenerateAndroidBuildActions(ctx android.ModuleContext) {
	j.testConfig = tradefed.AutoGenTestConfig(ctx, tradefed.AutoGenTestConfigOptions{
		TestConfigProp:       j.prebuiltTestProperties.Test_config,
		TestSuites:           j.prebuiltTestProperties.Test_suites,
		DeviceTemplate:       "${JavaTestConfigTemplate}",
		HostTemplate:         "${JavaHostTestConfigTemplate}",
		HostUnitTestTemplate: "${JavaHostUnitTestConfigTemplate}",
	})

	j.Import.GenerateAndroidBuildActions(ctx)
}

type testSdkMemberType struct {
	android.SdkMemberTypeBase
}

func (mt *testSdkMemberType) AddDependencies(ctx android.SdkDependencyContext, dependencyTag blueprint.DependencyTag, names []string) {
	ctx.AddVariationDependencies(nil, dependencyTag, names...)
}

func (mt *testSdkMemberType) IsInstance(module android.Module) bool {
	_, ok := module.(*Test)
	return ok
}

func (mt *testSdkMemberType) AddPrebuiltModule(ctx android.SdkMemberContext, member android.SdkMember) android.BpModule {
	return ctx.SnapshotBuilder().AddPrebuiltModule(member, "java_test_import")
}

func (mt *testSdkMemberType) CreateVariantPropertiesStruct() android.SdkMemberProperties {
	return &testSdkMemberProperties{}
}

type testSdkMemberProperties struct {
	android.SdkMemberPropertiesBase

	JarToExport android.Path
	TestConfig  android.Path
}

func (p *testSdkMemberProperties) PopulateFromVariant(ctx android.SdkMemberContext, variant android.Module) {
	test := variant.(*Test)

	implementationJars := test.ImplementationJars()
	if len(implementationJars) != 1 {
		panic(fmt.Errorf("there must be only one implementation jar from %q", test.Name()))
	}

	p.JarToExport = implementationJars[0]
	p.TestConfig = test.testConfig
}

func (p *testSdkMemberProperties) AddToPropertySet(ctx android.SdkMemberContext, propertySet android.BpPropertySet) {
	builder := ctx.SnapshotBuilder()

	exportedJar := p.JarToExport
	if exportedJar != nil {
		snapshotRelativeJavaLibPath := sdkSnapshotFilePathForJar(ctx, p.OsPrefix(), ctx.Name())
		builder.CopyToSnapshot(exportedJar, snapshotRelativeJavaLibPath)

		propertySet.AddProperty("jars", []string{snapshotRelativeJavaLibPath})
	}

	testConfig := p.TestConfig
	if testConfig != nil {
		snapshotRelativeTestConfigPath := sdkSnapshotFilePathForMember(p.OsPrefix(), ctx.Name(), testConfigSuffix)
		builder.CopyToSnapshot(testConfig, snapshotRelativeTestConfigPath)
		propertySet.AddProperty("test_config", snapshotRelativeTestConfigPath)
	}
}

// java_test builds a and links sources into a `.jar` file for the device, and possibly for the host as well, and
// creates an `AndroidTest.xml` file to allow running the test with `atest` or a `TEST_MAPPING` file.
//
// By default, a java_test has a single variant that produces a `.jar` file containing `classes.dex` files that were
// compiled against the device bootclasspath.
//
// Specifying `host_supported: true` will produce two variants, one compiled against the device bootclasspath and one
// compiled against the host bootclasspath.
func TestFactory() android.Module {
	module := &Test{}

	module.addHostAndDeviceProperties()
	module.AddProperties(&module.testProperties)

	module.Module.properties.Installable = proptools.BoolPtr(true)
	module.Module.dexpreopter.isTest = true
	module.Module.linter.properties.Lint.Test = proptools.BoolPtr(true)

	InitJavaModule(module, android.HostAndDeviceSupported)
	return module
}

// java_test_helper_library creates a java library and makes sure that it is added to the appropriate test suite.
func TestHelperLibraryFactory() android.Module {
	module := &TestHelperLibrary{}

	module.addHostAndDeviceProperties()
	module.AddProperties(&module.testHelperLibraryProperties)

	module.Module.properties.Installable = proptools.BoolPtr(true)
	module.Module.dexpreopter.isTest = true
	module.Module.linter.properties.Lint.Test = proptools.BoolPtr(true)

	InitJavaModule(module, android.HostAndDeviceSupported)
	return module
}

// java_test_import imports one or more `.jar` files into the build graph as if they were built by a java_test module
// and makes sure that it is added to the appropriate test suite.
//
// By default, a java_test_import has a single variant that expects a `.jar` file containing `.class` files that were
// compiled against an Android classpath.
//
// Specifying `host_supported: true` will produce two variants, one for use as a dependency of device modules and one
// for host modules.
func JavaTestImportFactory() android.Module {
	module := &JavaTestImport{}

	module.AddProperties(
		&module.Import.properties,
		&module.prebuiltTestProperties)

	module.Import.properties.Installable = proptools.BoolPtr(true)

	android.InitPrebuiltModule(module, &module.properties.Jars)
	android.InitApexModule(module)
	InitJavaModule(module, android.HostAndDeviceSupported)
	return module
}

// java_test_host builds a and links sources into a `.jar` file for the host, and creates an `AndroidTest.xml` file to
// allow running the test with `atest` or a `TEST_MAPPING` file.
//
// A java_test_host has a single variant that produces a `.jar` file containing `.class` files that were
// compiled against the host bootclasspath.
func TestHostFactory() android.Module {
	module := &TestHost{}

	module.addHostProperties()
	module.AddProperties(&module.testProperties)
	module.AddProperties(&module.testHostProperties)

	InitTestHost(
		module,
		proptools.BoolPtr(true),
		nil,
		nil)

	android.InitBazelModule(module)

	InitJavaModuleMultiTargets(module, android.HostSupported)

	return module
}

func InitTestHost(th *TestHost, installable *bool, testSuites []string, autoGenConfig *bool) {
	th.properties.Installable = installable
	th.testProperties.Auto_gen_config = autoGenConfig
	th.testProperties.Test_suites = testSuites
}

//
// Java Binaries (.jar file plus wrapper script)
//

type binaryProperties struct {
	// installable script to execute the resulting jar
	Wrapper *string `android:"path,arch_variant"`

	// Name of the class containing main to be inserted into the manifest as Main-Class.
	Main_class *string

	// Names of modules containing JNI libraries that should be installed alongside the host
	// variant of the binary.
	Jni_libs []string `android:"arch_variant"`
}

type Binary struct {
	Library

	binaryProperties binaryProperties

	isWrapperVariant bool

	wrapperFile android.Path
	binaryFile  android.InstallPath
}

func (j *Binary) HostToolPath() android.OptionalPath {
	return android.OptionalPathForPath(j.binaryFile)
}

func (j *Binary) GenerateAndroidBuildActions(ctx android.ModuleContext) {
	j.stem = proptools.StringDefault(j.overridableDeviceProperties.Stem, ctx.ModuleName())

	if ctx.Arch().ArchType == android.Common {
		// Compile the jar
		if j.binaryProperties.Main_class != nil {
			if j.properties.Manifest != nil {
				ctx.PropertyErrorf("main_class", "main_class cannot be used when manifest is set")
			}
			manifestFile := android.PathForModuleOut(ctx, "manifest.txt")
			GenerateMainClassManifest(ctx, manifestFile, String(j.binaryProperties.Main_class))
			j.overrideManifest = android.OptionalPathForPath(manifestFile)
		}

		j.Library.GenerateAndroidBuildActions(ctx)
	} else {
		// Handle the binary wrapper
		j.isWrapperVariant = true

		if j.binaryProperties.Wrapper != nil {
			j.wrapperFile = android.PathForModuleSrc(ctx, *j.binaryProperties.Wrapper)
		} else {
			if ctx.Windows() {
				ctx.PropertyErrorf("wrapper", "wrapper is required for Windows")
			}

			if ctx.Device() {
				// device binary should have a main_class property if it does not
				// have a specific wrapper, so that a default wrapper can
				// be generated for it.
				if j.binaryProperties.Main_class == nil {
					ctx.PropertyErrorf("main_class", "main_class property "+
						"is required for device binary if no default wrapper is assigned")
				} else {
					wrapper := android.PathForModuleOut(ctx, ctx.ModuleName()+".sh")
					jarName := j.Stem() + ".jar"
					partition := j.PartitionTag(ctx.DeviceConfig())
					ctx.Build(pctx, android.BuildParams{
						Rule:   deviceBinaryWrapper,
						Output: wrapper,
						Args: map[string]string{
							"jar_name":   jarName,
							"partition":  partition,
							"main_class": String(j.binaryProperties.Main_class),
						},
					})
					j.wrapperFile = wrapper
				}
			} else {
				j.wrapperFile = android.PathForSource(ctx, "build/soong/scripts/jar-wrapper.sh")
			}
		}

		ext := ""
		if ctx.Windows() {
			ext = ".bat"
		}

		// The host installation rules make the installed wrapper depend on all the dependencies
		// of the wrapper variant, which will include the common variant's jar file and any JNI
		// libraries.  This is verified by TestBinary.
		j.binaryFile = ctx.InstallExecutable(android.PathForModuleInstall(ctx, "bin"),
			ctx.ModuleName()+ext, j.wrapperFile)
	}
}

func (j *Binary) DepsMutator(ctx android.BottomUpMutatorContext) {
	if ctx.Arch().ArchType == android.Common {
		j.deps(ctx)
	}
	if ctx.Arch().ArchType != android.Common {
		// These dependencies ensure the host installation rules will install the jar file and
		// the jni libraries when the wrapper is installed.
		ctx.AddVariationDependencies(nil, jniInstallTag, j.binaryProperties.Jni_libs...)
		ctx.AddVariationDependencies(
			[]blueprint.Variation{{Mutator: "arch", Variation: android.CommonArch.String()}},
			binaryInstallTag, ctx.ModuleName())
	}
}

// java_binary builds a `.jar` file and a shell script that executes it for the device, and possibly for the host
// as well.
//
// By default, a java_binary has a single variant that produces a `.jar` file containing `classes.dex` files that were
// compiled against the device bootclasspath.
//
// Specifying `host_supported: true` will produce two variants, one compiled against the device bootclasspath and one
// compiled against the host bootclasspath.
func BinaryFactory() android.Module {
	module := &Binary{}

	module.addHostAndDeviceProperties()
	module.AddProperties(&module.binaryProperties)

	module.Module.properties.Installable = proptools.BoolPtr(true)

	android.InitAndroidArchModule(module, android.HostAndDeviceSupported, android.MultilibCommonFirst)
	android.InitDefaultableModule(module)
	android.InitBazelModule(module)

	return module
}

// java_binary_host builds a `.jar` file and a shell script that executes it for the host.
//
// A java_binary_host has a single variant that produces a `.jar` file containing `.class` files that were
// compiled against the host bootclasspath.
func BinaryHostFactory() android.Module {
	module := &Binary{}

	module.addHostProperties()
	module.AddProperties(&module.binaryProperties)

	module.Module.properties.Installable = proptools.BoolPtr(true)

	android.InitAndroidArchModule(module, android.HostSupported, android.MultilibCommonFirst)
	android.InitDefaultableModule(module)
	android.InitBazelModule(module)
	return module
}

type JavaApiContribution struct {
	android.ModuleBase
	android.DefaultableModuleBase

	properties struct {
		// name of the API surface
		Api_surface *string

		// relative path to the API signature text file
		Api_file *string `android:"path"`
	}
}

func ApiContributionFactory() android.Module {
	module := &JavaApiContribution{}
	android.InitAndroidModule(module)
	android.InitDefaultableModule(module)
	module.AddProperties(&module.properties)
	return module
}

type JavaApiImportInfo struct {
	ApiFile android.Path
}

var JavaApiImportProvider = blueprint.NewProvider(JavaApiImportInfo{})

func (ap *JavaApiContribution) GenerateAndroidBuildActions(ctx android.ModuleContext) {
	var apiFile android.Path = nil
	if apiFileString := ap.properties.Api_file; apiFileString != nil {
		apiFile = android.PathForModuleSrc(ctx, String(apiFileString))
	}

	ctx.SetProvider(JavaApiImportProvider, JavaApiImportInfo{
		ApiFile: apiFile,
	})
}

type ApiLibrary struct {
	android.ModuleBase
	android.DefaultableModuleBase

	hiddenAPI
	dexer

	properties JavaApiLibraryProperties

	stubsSrcJar               android.WritablePath
	stubsJar                  android.WritablePath
	stubsJarWithoutStaticLibs android.WritablePath
	extractedSrcJar           android.WritablePath
	// .dex of stubs, used for hiddenapi processing
	dexJarFile OptionalDexJarPath
}

type JavaApiLibraryProperties struct {
	// name of the API surface
	Api_surface *string

	// list of Java API contribution modules that consists this API surface
	// This is a list of Soong modules
	Api_contributions []string

	// list of api.txt files relative to this directory that contribute to the
	// API surface.
	// This is a list of relative paths
	Api_files []string `android:"path"`

	// List of flags to be passed to the javac compiler to generate jar file
	Javacflags []string

	// List of shared java libs that this module has dependencies to and
	// should be passed as classpath in javac invocation
	Libs []string

	// List of java libs that this module has static dependencies to and will be
	// merge zipped after metalava invocation
	Static_libs []string

	// Java Api library to provide the full API surface stub jar file.
	// If this property is set, the stub jar of this module is created by
	// extracting the compiled class files provided by the
	// full_api_surface_stub module.
	Full_api_surface_stub *string

	// Version of previously released API file for compatibility check.
	Previous_api *string `android:"path"`
}

func ApiLibraryFactory() android.Module {
	module := &ApiLibrary{}
	android.InitAndroidArchModule(module, android.DeviceSupported, android.MultilibCommon)
	module.AddProperties(&module.properties)
	android.InitDefaultableModule(module)
	return module
}

func (al *ApiLibrary) ApiSurface() *string {
	return al.properties.Api_surface
}

func (al *ApiLibrary) StubsJar() android.Path {
	return al.stubsJar
}

func metalavaStubCmd(ctx android.ModuleContext, rule *android.RuleBuilder,
	srcs android.Paths, homeDir android.WritablePath) *android.RuleBuilderCommand {
	rule.Command().Text("rm -rf").Flag(homeDir.String())
	rule.Command().Text("mkdir -p").Flag(homeDir.String())

	cmd := rule.Command()
	cmd.FlagWithArg("ANDROID_PREFS_ROOT=", homeDir.String())

	if metalavaUseRbe(ctx) {
		rule.Remoteable(android.RemoteRuleSupports{RBE: true})
		execStrategy := ctx.Config().GetenvWithDefault("RBE_METALAVA_EXEC_STRATEGY", remoteexec.LocalExecStrategy)
		labels := map[string]string{"type": "tool", "name": "metalava"}

		pool := ctx.Config().GetenvWithDefault("RBE_METALAVA_POOL", "java16")
		rule.Rewrapper(&remoteexec.REParams{
			Labels:          labels,
			ExecStrategy:    execStrategy,
			ToolchainInputs: []string{config.JavaCmd(ctx).String()},
			Platform:        map[string]string{remoteexec.PoolKey: pool},
		})
	}

	cmd.BuiltTool("metalava").ImplicitTool(ctx.Config().HostJavaToolPath(ctx, "metalava.jar")).
		Flag(config.JavacVmFlags).
		Flag("-J--add-opens=java.base/java.util=ALL-UNNAMED").
		FlagWithInputList("--source-files ", srcs, " ")

	cmd.Flag("--color").
		Flag("--quiet").
		Flag("--format=v2").
		Flag("--include-annotations").
		// The flag makes nullability issues as warnings rather than errors by replacing
		// @Nullable/@NonNull in the listed packages APIs with @RecentlyNullable/@RecentlyNonNull,
		// and these packages are meant to have everything annotated
		// @RecentlyNullable/@RecentlyNonNull.
		FlagWithArg("--force-convert-to-warning-nullability-annotations ", "+*:-android.*:+android.icu.*:-dalvik.*").
		FlagWithArg("--repeat-errors-max ", "10").
		FlagWithArg("--hide ", "UnresolvedImport").
		FlagWithArg("--hide ", "InvalidNullabilityOverride").
		FlagWithArg("--hide ", "ChangedDefault")

	// Force metalava to ignore classes on the classpath when an API file contains missing classes.
	// See b/285140653 for more information.
	cmd.FlagWithArg("--api-class-resolution ", "api")

	// Force metalava to sort overloaded methods by their order in the source code.
	// See b/285312164 for more information.
	cmd.FlagWithArg("--api-overloaded-method-order ", "source")

	return cmd
}

func (al *ApiLibrary) HeaderJars() android.Paths {
	return android.Paths{al.stubsJar}
}

func (al *ApiLibrary) OutputDirAndDeps() (android.Path, android.Paths) {
	return nil, nil
}

func (al *ApiLibrary) stubsFlags(ctx android.ModuleContext, cmd *android.RuleBuilderCommand, stubsDir android.OptionalPath) {
	if stubsDir.Valid() {
		cmd.FlagWithArg("--stubs ", stubsDir.String())
	}
}

// This method extracts the stub class files from the stub jar file provided
// from full_api_surface_stub module instead of compiling the srcjar generated from invoking metalava.
// This method is used because metalava can generate compilable from-text stubs only when
// the codebase encompasses all classes listed in the input API text file, and a class can extend
// a class that is not within the same API domain.
func (al *ApiLibrary) extractApiSrcs(ctx android.ModuleContext, rule *android.RuleBuilder, stubsDir android.OptionalPath, fullApiSurfaceStubJar android.Path) {
	classFilesList := android.PathForModuleOut(ctx, "metalava", "classes.txt")
	unzippedSrcJarDir := android.PathForModuleOut(ctx, "metalava", "unzipDir")

	rule.Command().
		BuiltTool("list_files").
		Text(stubsDir.String()).
		FlagWithOutput("--out ", classFilesList).
		FlagWithArg("--extensions ", ".java").
		FlagWithArg("--root ", unzippedSrcJarDir.String()).
		Flag("--classes")

	rule.Command().
		Text("unzip").
		Flag("-q").
		Input(fullApiSurfaceStubJar).
		FlagWithArg("-d ", unzippedSrcJarDir.String())

	rule.Command().
		BuiltTool("soong_zip").
		Flag("-jar").
		Flag("-write_if_changed").
		Flag("-ignore_missing_files").
		FlagWithArg("-C ", unzippedSrcJarDir.String()).
		FlagWithInput("-l ", classFilesList).
		FlagWithOutput("-o ", al.stubsJarWithoutStaticLibs)
}

func (al *ApiLibrary) DepsMutator(ctx android.BottomUpMutatorContext) {
	apiContributions := al.properties.Api_contributions
	for _, apiContributionName := range apiContributions {
		ctx.AddDependency(ctx.Module(), javaApiContributionTag, apiContributionName)
	}
	ctx.AddVariationDependencies(nil, libTag, al.properties.Libs...)
	ctx.AddVariationDependencies(nil, staticLibTag, al.properties.Static_libs...)
	if al.properties.Full_api_surface_stub != nil {
		ctx.AddVariationDependencies(nil, depApiSrcsTag, String(al.properties.Full_api_surface_stub))
	}
}

// API signature file names sorted from
// the narrowest api scope to the widest api scope
var scopeOrderedSourceFileNames = allApiScopes.Strings(
	func(s *apiScope) string { return s.apiFilePrefix + "current.txt" })

func (al *ApiLibrary) sortApiFilesByApiScope(ctx android.ModuleContext, srcFiles android.Paths) android.Paths {
	sortedSrcFiles := android.Paths{}

	for _, scopeSourceFileName := range scopeOrderedSourceFileNames {
		for _, sourceFileName := range srcFiles {
			if sourceFileName.Base() == scopeSourceFileName {
				sortedSrcFiles = append(sortedSrcFiles, sourceFileName)
			}
		}
	}
	if len(srcFiles) != len(sortedSrcFiles) {
		ctx.ModuleErrorf("Unrecognizable source file found within %s", srcFiles)
	}

	return sortedSrcFiles
}

func (al *ApiLibrary) GenerateAndroidBuildActions(ctx android.ModuleContext) {

	rule := android.NewRuleBuilder(pctx, ctx)

	rule.Sbox(android.PathForModuleOut(ctx, "metalava"),
		android.PathForModuleOut(ctx, "metalava.sbox.textproto")).
		SandboxInputs()

	var stubsDir android.OptionalPath
	stubsDir = android.OptionalPathForPath(android.PathForModuleOut(ctx, "metalava", "stubsDir"))
	rule.Command().Text("rm -rf").Text(stubsDir.String())
	rule.Command().Text("mkdir -p").Text(stubsDir.String())

	homeDir := android.PathForModuleOut(ctx, "metalava", "home")

	var srcFiles android.Paths
	var classPaths android.Paths
	var staticLibs android.Paths
	var depApiSrcsStubsJar android.Path
	ctx.VisitDirectDeps(func(dep android.Module) {
		tag := ctx.OtherModuleDependencyTag(dep)
		switch tag {
		case javaApiContributionTag:
			provider := ctx.OtherModuleProvider(dep, JavaApiImportProvider).(JavaApiImportInfo)
			providerApiFile := provider.ApiFile
			if providerApiFile == nil && !ctx.Config().AllowMissingDependencies() {
				ctx.ModuleErrorf("Error: %s has an empty api file.", dep.Name())
			}
			srcFiles = append(srcFiles, android.PathForSource(ctx, providerApiFile.String()))
		case libTag:
			provider := ctx.OtherModuleProvider(dep, JavaInfoProvider).(JavaInfo)
			classPaths = append(classPaths, provider.HeaderJars...)
		case staticLibTag:
			provider := ctx.OtherModuleProvider(dep, JavaInfoProvider).(JavaInfo)
			staticLibs = append(staticLibs, provider.HeaderJars...)
		case depApiSrcsTag:
			provider := ctx.OtherModuleProvider(dep, JavaInfoProvider).(JavaInfo)
			depApiSrcsStubsJar = provider.HeaderJars[0]
		}
	})

	// Add the api_files inputs
	for _, api := range al.properties.Api_files {
		srcFiles = append(srcFiles, android.PathForModuleSrc(ctx, api))
	}

	if srcFiles == nil && !ctx.Config().AllowMissingDependencies() {
		ctx.ModuleErrorf("Error: %s has an empty api file.", ctx.ModuleName())
	}

	srcFiles = al.sortApiFilesByApiScope(ctx, srcFiles)

	cmd := metalavaStubCmd(ctx, rule, srcFiles, homeDir)

	al.stubsFlags(ctx, cmd, stubsDir)

	migratingNullability := String(al.properties.Previous_api) != ""
	if migratingNullability {
		previousApi := android.PathForModuleSrc(ctx, String(al.properties.Previous_api))
		cmd.FlagWithInput("--migrate-nullness ", previousApi)
	}

	al.stubsSrcJar = android.PathForModuleOut(ctx, "metalava", ctx.ModuleName()+"-"+"stubs.srcjar")
	al.stubsJarWithoutStaticLibs = android.PathForModuleOut(ctx, "metalava", "stubs.jar")
	al.stubsJar = android.PathForModuleOut(ctx, ctx.ModuleName(), fmt.Sprintf("%s.jar", ctx.ModuleName()))

	if depApiSrcsStubsJar != nil {
		al.extractApiSrcs(ctx, rule, stubsDir, depApiSrcsStubsJar)
	}
	rule.Command().
		BuiltTool("soong_zip").
		Flag("-write_if_changed").
		Flag("-jar").
		FlagWithOutput("-o ", al.stubsSrcJar).
		FlagWithArg("-C ", stubsDir.String()).
		FlagWithArg("-D ", stubsDir.String())

	rule.Build("metalava", "metalava merged")

	if depApiSrcsStubsJar == nil {
		var flags javaBuilderFlags
		flags.javaVersion = getStubsJavaVersion()
		flags.javacFlags = strings.Join(al.properties.Javacflags, " ")
		flags.classpath = classpath(classPaths)

		annoSrcJar := android.PathForModuleOut(ctx, ctx.ModuleName(), "anno.srcjar")

		TransformJavaToClasses(ctx, al.stubsJarWithoutStaticLibs, 0, android.Paths{},
			android.Paths{al.stubsSrcJar}, annoSrcJar, flags, android.Paths{})
	}

	builder := android.NewRuleBuilder(pctx, ctx)
	builder.Command().
		BuiltTool("merge_zips").
		Output(al.stubsJar).
		Inputs(android.Paths{al.stubsJarWithoutStaticLibs}).
		Inputs(staticLibs)
	builder.Build("merge_zips", "merge jar files")

	// compile stubs to .dex for hiddenapi processing
	dexParams := &compileDexParams{
		flags:         javaBuilderFlags{},
		sdkVersion:    al.SdkVersion(ctx),
		minSdkVersion: al.MinSdkVersion(ctx),
		classesJar:    al.stubsJar,
		jarName:       ctx.ModuleName() + ".jar",
	}
	dexOutputFile := al.dexer.compileDex(ctx, dexParams)
	uncompressed := true
	al.initHiddenAPI(ctx, makeDexJarPathFromPath(dexOutputFile), al.stubsJar, &uncompressed)
	dexOutputFile = al.hiddenAPIEncodeDex(ctx, dexOutputFile)
	al.dexJarFile = makeDexJarPathFromPath(dexOutputFile)

	ctx.Phony(ctx.ModuleName(), al.stubsJar)

	ctx.SetProvider(JavaInfoProvider, JavaInfo{
		HeaderJars:                     android.PathsIfNonNil(al.stubsJar),
		ImplementationAndResourcesJars: android.PathsIfNonNil(al.stubsJar),
		ImplementationJars:             android.PathsIfNonNil(al.stubsJar),
		AidlIncludeDirs:                android.Paths{},
		// No aconfig libraries on api libraries
	})
}

func (al *ApiLibrary) DexJarBuildPath() OptionalDexJarPath {
	return al.dexJarFile
}

func (al *ApiLibrary) DexJarInstallPath() android.Path {
	return al.dexJarFile.Path()
}

func (al *ApiLibrary) ClassLoaderContexts() dexpreopt.ClassLoaderContextMap {
	return nil
}

// java_api_library constitutes the sdk, and does not build against one
func (al *ApiLibrary) SdkVersion(ctx android.EarlyModuleContext) android.SdkSpec {
	return android.SdkSpecNone
}

// java_api_library is always at "current". Return FutureApiLevel
func (al *ApiLibrary) MinSdkVersion(ctx android.EarlyModuleContext) android.ApiLevel {
	return android.FutureApiLevel
}

// implement the following interfaces for hiddenapi processing
var _ hiddenAPIModule = (*ApiLibrary)(nil)
var _ UsesLibraryDependency = (*ApiLibrary)(nil)

//
// Java prebuilts
//

type ImportProperties struct {
	Jars []string `android:"path,arch_variant"`

	// The version of the SDK that the source prebuilt file was built against. Defaults to the
	// current version if not specified.
	Sdk_version *string

	// The minimum version of the SDK that this module supports. Defaults to sdk_version if not
	// specified.
	Min_sdk_version *string

	// The max sdk version placeholder used to replace maxSdkVersion attributes on permission
	// and uses-permission tags in manifest_fixer.
	Replace_max_sdk_version_placeholder *string

	Installable *bool

	// If not empty, classes are restricted to the specified packages and their sub-packages.
	Permitted_packages []string

	// List of shared java libs that this module has dependencies to
	Libs []string

	// List of files to remove from the jar file(s)
	Exclude_files []string

	// List of directories to remove from the jar file(s)
	Exclude_dirs []string

	// if set to true, run Jetifier against .jar file. Defaults to false.
	Jetifier *bool

	// set the name of the output
	Stem *string

	Aidl struct {
		// directories that should be added as include directories for any aidl sources of modules
		// that depend on this module, as well as to aidl for this module.
		Export_include_dirs []string
	}
}

type Import struct {
	android.ModuleBase
	android.DefaultableModuleBase
	android.ApexModuleBase
	android.BazelModuleBase
	prebuilt android.Prebuilt

	// Functionality common to Module and Import.
	embeddableInModuleAndImport

	hiddenAPI
	dexer
	dexpreopter

	properties ImportProperties

	// output file containing classes.dex and resources
	dexJarFile        OptionalDexJarPath
	dexJarInstallFile android.Path

	combinedClasspathFile android.Path
	classLoaderContexts   dexpreopt.ClassLoaderContextMap
	exportAidlIncludeDirs android.Paths

	hideApexVariantFromMake bool

	sdkVersion    android.SdkSpec
	minSdkVersion android.ApiLevel
}

var _ PermittedPackagesForUpdatableBootJars = (*Import)(nil)

func (j *Import) PermittedPackagesForUpdatableBootJars() []string {
	return j.properties.Permitted_packages
}

func (j *Import) SdkVersion(ctx android.EarlyModuleContext) android.SdkSpec {
	return android.SdkSpecFrom(ctx, String(j.properties.Sdk_version))
}

func (j *Import) SystemModules() string {
	return "none"
}

func (j *Import) MinSdkVersion(ctx android.EarlyModuleContext) android.ApiLevel {
	if j.properties.Min_sdk_version != nil {
		return android.ApiLevelFrom(ctx, *j.properties.Min_sdk_version)
	}
	return j.SdkVersion(ctx).ApiLevel
}

func (j *Import) ReplaceMaxSdkVersionPlaceholder(ctx android.EarlyModuleContext) android.ApiLevel {
	if j.properties.Replace_max_sdk_version_placeholder != nil {
		return android.ApiLevelFrom(ctx, *j.properties.Replace_max_sdk_version_placeholder)
	}
	// Default is PrivateApiLevel
	return android.SdkSpecPrivate.ApiLevel
}

func (j *Import) TargetSdkVersion(ctx android.EarlyModuleContext) android.ApiLevel {
	return j.SdkVersion(ctx).ApiLevel
}

func (j *Import) Prebuilt() *android.Prebuilt {
	return &j.prebuilt
}

func (j *Import) PrebuiltSrcs() []string {
	return j.properties.Jars
}

func (j *Import) Name() string {
	return j.prebuilt.Name(j.ModuleBase.Name())
}

func (j *Import) Stem() string {
	return proptools.StringDefault(j.properties.Stem, j.ModuleBase.Name())
}

func (a *Import) JacocoReportClassesFile() android.Path {
	return nil
}

func (j *Import) LintDepSets() LintDepSets {
	return LintDepSets{}
}

func (j *Import) getStrictUpdatabilityLinting() bool {
	return false
}

func (j *Import) setStrictUpdatabilityLinting(bool) {
}

func (j *Import) DepsMutator(ctx android.BottomUpMutatorContext) {
	ctx.AddVariationDependencies(nil, libTag, j.properties.Libs...)

	if ctx.Device() && Bool(j.dexProperties.Compile_dex) {
		sdkDeps(ctx, android.SdkContext(j), j.dexer)
	}
}

func (j *Import) commonBuildActions(ctx android.ModuleContext) {
	//TODO(b/231322772) these should come from Bazel once available
	j.sdkVersion = j.SdkVersion(ctx)
	j.minSdkVersion = j.MinSdkVersion(ctx)

	if !ctx.Provider(android.ApexInfoProvider).(android.ApexInfo).IsForPlatform() {
		j.hideApexVariantFromMake = true
	}

	if ctx.Windows() {
		j.HideFromMake()
	}
}

func (j *Import) GenerateAndroidBuildActions(ctx android.ModuleContext) {
	j.commonBuildActions(ctx)

	jars := android.PathsForModuleSrc(ctx, j.properties.Jars)

	jarName := j.Stem() + ".jar"
	outputFile := android.PathForModuleOut(ctx, "combined", jarName)
	TransformJarsToJar(ctx, outputFile, "for prebuilts", jars, android.OptionalPath{},
		false, j.properties.Exclude_files, j.properties.Exclude_dirs)
	if Bool(j.properties.Jetifier) {
		inputFile := outputFile
		outputFile = android.PathForModuleOut(ctx, "jetifier", jarName)
		TransformJetifier(ctx, outputFile, inputFile)
	}
	j.combinedClasspathFile = outputFile
	j.classLoaderContexts = make(dexpreopt.ClassLoaderContextMap)

	var flags javaBuilderFlags

	j.collectTransitiveHeaderJars(ctx)
	ctx.VisitDirectDeps(func(module android.Module) {
		tag := ctx.OtherModuleDependencyTag(module)
		if ctx.OtherModuleHasProvider(module, JavaInfoProvider) {
			dep := ctx.OtherModuleProvider(module, JavaInfoProvider).(JavaInfo)
			switch tag {
			case libTag, sdkLibTag:
				flags.classpath = append(flags.classpath, dep.HeaderJars...)
				flags.dexClasspath = append(flags.dexClasspath, dep.HeaderJars...)
			case staticLibTag:
				flags.classpath = append(flags.classpath, dep.HeaderJars...)
			case bootClasspathTag:
				flags.bootClasspath = append(flags.bootClasspath, dep.HeaderJars...)
			}
		} else if dep, ok := module.(SdkLibraryDependency); ok {
			switch tag {
			case libTag, sdkLibTag:
				flags.classpath = append(flags.classpath, dep.SdkHeaderJars(ctx, j.SdkVersion(ctx))...)
			}
		}

		addCLCFromDep(ctx, module, j.classLoaderContexts)
	})

	j.maybeInstall(ctx, jarName, outputFile)

	j.exportAidlIncludeDirs = android.PathsForModuleSrc(ctx, j.properties.Aidl.Export_include_dirs)

	if ctx.Device() {
		// If this is a variant created for a prebuilt_apex then use the dex implementation jar
		// obtained from the associated deapexer module.
		ai := ctx.Provider(android.ApexInfoProvider).(android.ApexInfo)
		if ai.ForPrebuiltApex {
			// Get the path of the dex implementation jar from the `deapexer` module.
			di := android.FindDeapexerProviderForModule(ctx)
			if di == nil {
				return // An error has been reported by FindDeapexerProviderForModule.
			}
			dexJarFileApexRootRelative := apexRootRelativePathToJavaLib(j.BaseModuleName())
			if dexOutputPath := di.PrebuiltExportPath(dexJarFileApexRootRelative); dexOutputPath != nil {
				dexJarFile := makeDexJarPathFromPath(dexOutputPath)
				j.dexJarFile = dexJarFile
				installPath := android.PathForModuleInPartitionInstall(ctx, "apex", ai.ApexVariationName, apexRootRelativePathToJavaLib(j.BaseModuleName()))
				j.dexJarInstallFile = installPath

				j.dexpreopter.installPath = j.dexpreopter.getInstallPath(ctx, installPath)
				setUncompressDex(ctx, &j.dexpreopter, &j.dexer)
				j.dexpreopter.uncompressedDex = *j.dexProperties.Uncompress_dex

				if profilePath := di.PrebuiltExportPath(dexJarFileApexRootRelative + ".prof"); profilePath != nil {
					j.dexpreopter.inputProfilePathOnHost = profilePath
				}

				j.dexpreopt(ctx, dexOutputPath)

				// Initialize the hiddenapi structure.
				j.initHiddenAPI(ctx, dexJarFile, outputFile, j.dexProperties.Uncompress_dex)
			} else {
				// This should never happen as a variant for a prebuilt_apex is only created if the
				// prebuilt_apex has been configured to export the java library dex file.
				ctx.ModuleErrorf("internal error: no dex implementation jar available from prebuilt APEX %s", di.ApexModuleName())
			}
		} else if Bool(j.dexProperties.Compile_dex) {
			sdkDep := decodeSdkDep(ctx, android.SdkContext(j))
			if sdkDep.invalidVersion {
				ctx.AddMissingDependencies(sdkDep.bootclasspath)
				ctx.AddMissingDependencies(sdkDep.java9Classpath)
			} else if sdkDep.useFiles {
				// sdkDep.jar is actually equivalent to turbine header.jar.
				flags.classpath = append(flags.classpath, sdkDep.jars...)
			}

			// Dex compilation

			j.dexpreopter.installPath = j.dexpreopter.getInstallPath(
				ctx, android.PathForModuleInstall(ctx, "framework", jarName))
			setUncompressDex(ctx, &j.dexpreopter, &j.dexer)
			j.dexpreopter.uncompressedDex = *j.dexProperties.Uncompress_dex

			var dexOutputFile android.OutputPath
			dexParams := &compileDexParams{
				flags:         flags,
				sdkVersion:    j.SdkVersion(ctx),
				minSdkVersion: j.MinSdkVersion(ctx),
				classesJar:    outputFile,
				jarName:       jarName,
			}

			dexOutputFile = j.dexer.compileDex(ctx, dexParams)
			if ctx.Failed() {
				return
			}

			// Initialize the hiddenapi structure.
			j.initHiddenAPI(ctx, makeDexJarPathFromPath(dexOutputFile), outputFile, j.dexProperties.Uncompress_dex)

			// Encode hidden API flags in dex file.
			dexOutputFile = j.hiddenAPIEncodeDex(ctx, dexOutputFile)

			j.dexJarFile = makeDexJarPathFromPath(dexOutputFile)
			j.dexJarInstallFile = android.PathForModuleInstall(ctx, "framework", jarName)
		}
	}

	ctx.SetProvider(JavaInfoProvider, JavaInfo{
		HeaderJars:                     android.PathsIfNonNil(j.combinedClasspathFile),
		TransitiveLibsHeaderJars:       j.transitiveLibsHeaderJars,
		TransitiveStaticLibsHeaderJars: j.transitiveStaticLibsHeaderJars,
		ImplementationAndResourcesJars: android.PathsIfNonNil(j.combinedClasspathFile),
		ImplementationJars:             android.PathsIfNonNil(j.combinedClasspathFile),
		AidlIncludeDirs:                j.exportAidlIncludeDirs,
		// TODO(b/289117800): LOCAL_ACONFIG_FILES for prebuilts
	})
}

func (j *Import) maybeInstall(ctx android.ModuleContext, jarName string, outputFile android.Path) {
	if !Bool(j.properties.Installable) {
		return
	}

	var installDir android.InstallPath
	if ctx.InstallInTestcases() {
		var archDir string
		if !ctx.Host() {
			archDir = ctx.DeviceConfig().DeviceArch()
		}
		installDir = android.PathForModuleInstall(ctx, ctx.ModuleName(), archDir)
	} else {
		installDir = android.PathForModuleInstall(ctx, "framework")
	}
	ctx.InstallFile(installDir, jarName, outputFile)
}

func (j *Import) OutputFiles(tag string) (android.Paths, error) {
	switch tag {
	case "", ".jar":
		return android.Paths{j.combinedClasspathFile}, nil
	default:
		return nil, fmt.Errorf("unsupported module reference tag %q", tag)
	}
}

var _ android.OutputFileProducer = (*Import)(nil)

func (j *Import) HeaderJars() android.Paths {
	if j.combinedClasspathFile == nil {
		return nil
	}
	return android.Paths{j.combinedClasspathFile}
}

func (j *Import) ImplementationAndResourcesJars() android.Paths {
	if j.combinedClasspathFile == nil {
		return nil
	}
	return android.Paths{j.combinedClasspathFile}
}

func (j *Import) DexJarBuildPath() OptionalDexJarPath {
	return j.dexJarFile
}

func (j *Import) DexJarInstallPath() android.Path {
	return j.dexJarInstallFile
}

func (j *Import) ClassLoaderContexts() dexpreopt.ClassLoaderContextMap {
	return j.classLoaderContexts
}

var _ android.ApexModule = (*Import)(nil)

// Implements android.ApexModule
func (j *Import) DepIsInSameApex(ctx android.BaseModuleContext, dep android.Module) bool {
	return j.depIsInSameApex(ctx, dep)
}

// Implements android.ApexModule
func (j *Import) ShouldSupportSdkVersion(ctx android.BaseModuleContext,
	sdkVersion android.ApiLevel) error {
	sdkVersionSpec := j.SdkVersion(ctx)
	minSdkVersion := j.MinSdkVersion(ctx)
	if !minSdkVersion.Specified() {
		return fmt.Errorf("min_sdk_version is not specified")
	}
	// If the module is compiling against core (via sdk_version), skip comparison check.
	if sdkVersionSpec.Kind == android.SdkCore {
		return nil
	}
	if minSdkVersion.GreaterThan(sdkVersion) {
		return fmt.Errorf("newer SDK(%v)", minSdkVersion)
	}
	return nil
}

// requiredFilesFromPrebuiltApexForImport returns information about the files that a java_import or
// java_sdk_library_import with the specified base module name requires to be exported from a
// prebuilt_apex/apex_set.
func requiredFilesFromPrebuiltApexForImport(name string, d *dexpreopter) []string {
	dexJarFileApexRootRelative := apexRootRelativePathToJavaLib(name)
	// Add the dex implementation jar to the set of exported files.
	files := []string{
		dexJarFileApexRootRelative,
	}
	if BoolDefault(d.importDexpreoptProperties.Dex_preopt.Profile_guided, false) {
		files = append(files, dexJarFileApexRootRelative+".prof")
	}
	return files
}

// apexRootRelativePathToJavaLib returns the path, relative to the root of the apex's contents, for
// the java library with the specified name.
func apexRootRelativePathToJavaLib(name string) string {
	return filepath.Join("javalib", name+".jar")
}

var _ android.RequiredFilesFromPrebuiltApex = (*Import)(nil)

func (j *Import) RequiredFilesFromPrebuiltApex(_ android.BaseModuleContext) []string {
	name := j.BaseModuleName()
	return requiredFilesFromPrebuiltApexForImport(name, &j.dexpreopter)
}

// Add compile time check for interface implementation
var _ android.IDEInfo = (*Import)(nil)
var _ android.IDECustomizedModuleName = (*Import)(nil)

// Collect information for opening IDE project files in java/jdeps.go.

func (j *Import) IDEInfo(dpInfo *android.IdeInfo) {
	dpInfo.Jars = append(dpInfo.Jars, j.PrebuiltSrcs()...)
}

func (j *Import) IDECustomizedModuleName() string {
	// TODO(b/113562217): Extract the base module name from the Import name, often the Import name
	// has a prefix "prebuilt_". Remove the prefix explicitly if needed until we find a better
	// solution to get the Import name.
	return android.RemoveOptionalPrebuiltPrefix(j.Name())
}

var _ android.PrebuiltInterface = (*Import)(nil)

func (j *Import) IsInstallable() bool {
	return Bool(j.properties.Installable)
}

var _ DexpreopterInterface = (*Import)(nil)

// java_import imports one or more `.jar` files into the build graph as if they were built by a java_library module.
//
// By default, a java_import has a single variant that expects a `.jar` file containing `.class` files that were
// compiled against an Android classpath.
//
// Specifying `host_supported: true` will produce two variants, one for use as a dependency of device modules and one
// for host modules.
func ImportFactory() android.Module {
	module := &Import{}

	module.AddProperties(
		&module.properties,
		&module.dexer.dexProperties,
		&module.importDexpreoptProperties,
	)

	module.initModuleAndImport(module)

	module.dexProperties.Optimize.EnabledByDefault = false

	android.InitPrebuiltModule(module, &module.properties.Jars)
	android.InitApexModule(module)
	android.InitBazelModule(module)
	InitJavaModule(module, android.HostAndDeviceSupported)
	return module
}

// java_import imports one or more `.jar` files into the build graph as if they were built by a java_library_host
// module.
//
// A java_import_host has a single variant that expects a `.jar` file containing `.class` files that were
// compiled against a host bootclasspath.
func ImportFactoryHost() android.Module {
	module := &Import{}

	module.AddProperties(&module.properties)

	android.InitPrebuiltModule(module, &module.properties.Jars)
	android.InitApexModule(module)
	android.InitBazelModule(module)
	InitJavaModule(module, android.HostSupported)
	return module
}

// dex_import module

type DexImportProperties struct {
	Jars []string `android:"path"`

	// set the name of the output
	Stem *string
}

type DexImport struct {
	android.ModuleBase
	android.DefaultableModuleBase
	android.ApexModuleBase
	prebuilt android.Prebuilt

	properties DexImportProperties

	dexJarFile OptionalDexJarPath

	dexpreopter

	hideApexVariantFromMake bool
}

func (j *DexImport) Prebuilt() *android.Prebuilt {
	return &j.prebuilt
}

func (j *DexImport) PrebuiltSrcs() []string {
	return j.properties.Jars
}

func (j *DexImport) Name() string {
	return j.prebuilt.Name(j.ModuleBase.Name())
}

func (j *DexImport) Stem() string {
	return proptools.StringDefault(j.properties.Stem, j.ModuleBase.Name())
}

func (a *DexImport) JacocoReportClassesFile() android.Path {
	return nil
}

func (a *DexImport) LintDepSets() LintDepSets {
	return LintDepSets{}
}

func (j *DexImport) IsInstallable() bool {
	return true
}

func (j *DexImport) getStrictUpdatabilityLinting() bool {
	return false
}

func (j *DexImport) setStrictUpdatabilityLinting(bool) {
}

func (j *DexImport) GenerateAndroidBuildActions(ctx android.ModuleContext) {
	if len(j.properties.Jars) != 1 {
		ctx.PropertyErrorf("jars", "exactly one jar must be provided")
	}

	apexInfo := ctx.Provider(android.ApexInfoProvider).(android.ApexInfo)
	if !apexInfo.IsForPlatform() {
		j.hideApexVariantFromMake = true
	}

	j.dexpreopter.installPath = j.dexpreopter.getInstallPath(
		ctx, android.PathForModuleInstall(ctx, "framework", j.Stem()+".jar"))
	j.dexpreopter.uncompressedDex = shouldUncompressDex(ctx, &j.dexpreopter)

	inputJar := ctx.ExpandSource(j.properties.Jars[0], "jars")
	dexOutputFile := android.PathForModuleOut(ctx, ctx.ModuleName()+".jar")

	if j.dexpreopter.uncompressedDex {
		rule := android.NewRuleBuilder(pctx, ctx)

		temporary := android.PathForModuleOut(ctx, ctx.ModuleName()+".jar.unaligned")
		rule.Temporary(temporary)

		// use zip2zip to uncompress classes*.dex files
		rule.Command().
			BuiltTool("zip2zip").
			FlagWithInput("-i ", inputJar).
			FlagWithOutput("-o ", temporary).
			FlagWithArg("-0 ", "'classes*.dex'")

		// use zipalign to align uncompressed classes*.dex files
		rule.Command().
			BuiltTool("zipalign").
			Flag("-f").
			Text("4").
			Input(temporary).
			Output(dexOutputFile)

		rule.DeleteTemporaryFiles()

		rule.Build("uncompress_dex", "uncompress dex")
	} else {
		ctx.Build(pctx, android.BuildParams{
			Rule:   android.Cp,
			Input:  inputJar,
			Output: dexOutputFile,
		})
	}

	j.dexJarFile = makeDexJarPathFromPath(dexOutputFile)

	j.dexpreopt(ctx, dexOutputFile)

	if apexInfo.IsForPlatform() {
		ctx.InstallFile(android.PathForModuleInstall(ctx, "framework"),
			j.Stem()+".jar", dexOutputFile)
	}
}

func (j *DexImport) DexJarBuildPath() OptionalDexJarPath {
	return j.dexJarFile
}

var _ android.ApexModule = (*DexImport)(nil)

// Implements android.ApexModule
func (j *DexImport) ShouldSupportSdkVersion(ctx android.BaseModuleContext,
	sdkVersion android.ApiLevel) error {
	// we don't check prebuilt modules for sdk_version
	return nil
}

// dex_import imports a `.jar` file containing classes.dex files.
//
// A dex_import module cannot be used as a dependency of a java_* or android_* module, it can only be installed
// to the device.
func DexImportFactory() android.Module {
	module := &DexImport{}

	module.AddProperties(&module.properties)

	android.InitPrebuiltModule(module, &module.properties.Jars)
	android.InitApexModule(module)
	InitJavaModule(module, android.DeviceSupported)
	return module
}

// Defaults
type Defaults struct {
	android.ModuleBase
	android.DefaultsModuleBase
	android.ApexModuleBase
}

// java_defaults provides a set of properties that can be inherited by other java or android modules.
//
// A module can use the properties from a java_defaults module using `defaults: ["defaults_module_name"]`.  Each
// property in the defaults module that exists in the depending module will be prepended to the depending module's
// value for that property.
//
// Example:
//
//	java_defaults {
//	    name: "example_defaults",
//	    srcs: ["common/**/*.java"],
//	    javacflags: ["-Xlint:all"],
//	    aaptflags: ["--auto-add-overlay"],
//	}
//
//	java_library {
//	    name: "example",
//	    defaults: ["example_defaults"],
//	    srcs: ["example/**/*.java"],
//	}
//
// is functionally identical to:
//
//	java_library {
//	    name: "example",
//	    srcs: [
//	        "common/**/*.java",
//	        "example/**/*.java",
//	    ],
//	    javacflags: ["-Xlint:all"],
//	}
func DefaultsFactory() android.Module {
	module := &Defaults{}

	module.AddProperties(
		&CommonProperties{},
		&DeviceProperties{},
		&OverridableDeviceProperties{},
		&DexProperties{},
		&DexpreoptProperties{},
		&android.ProtoProperties{},
		&aaptProperties{},
		&androidLibraryProperties{},
		&appProperties{},
		&appTestProperties{},
		&overridableAppProperties{},
		&hostTestProperties{},
		&testProperties{},
		&ImportProperties{},
		&AARImportProperties{},
		&sdkLibraryProperties{},
		&commonToSdkLibraryAndImportProperties{},
		&DexImportProperties{},
		&android.ApexProperties{},
		&RuntimeResourceOverlayProperties{},
		&LintProperties{},
		&appTestHelperAppProperties{},
		&JavaApiLibraryProperties{},
	)

	android.InitDefaultsModule(module)
	return module
}

func kytheExtractJavaFactory() android.Singleton {
	return &kytheExtractJavaSingleton{}
}

type kytheExtractJavaSingleton struct {
}

func (ks *kytheExtractJavaSingleton) GenerateBuildActions(ctx android.SingletonContext) {
	var xrefTargets android.Paths
	ctx.VisitAllModules(func(module android.Module) {
		if javaModule, ok := module.(xref); ok {
			xrefTargets = append(xrefTargets, javaModule.XrefJavaFiles()...)
		}
	})
	// TODO(asmundak): perhaps emit a rule to output a warning if there were no xrefTargets
	if len(xrefTargets) > 0 {
		ctx.Phony("xref_java", xrefTargets...)
	}
}

var Bool = proptools.Bool
var BoolDefault = proptools.BoolDefault
var String = proptools.String
var inList = android.InList[string]

// Add class loader context (CLC) of a given dependency to the current CLC.
func addCLCFromDep(ctx android.ModuleContext, depModule android.Module,
	clcMap dexpreopt.ClassLoaderContextMap) {

	dep, ok := depModule.(UsesLibraryDependency)
	if !ok {
		return
	}

	depName := android.RemoveOptionalPrebuiltPrefix(ctx.OtherModuleName(depModule))

	var sdkLib *string
	if lib, ok := depModule.(SdkLibraryDependency); ok && lib.sharedLibrary() {
		// A shared SDK library. This should be added as a top-level CLC element.
		sdkLib = &depName
	} else if ulib, ok := depModule.(ProvidesUsesLib); ok {
		// A non-SDK library disguised as an SDK library by the means of `provides_uses_lib`
		// property. This should be handled in the same way as a shared SDK library.
		sdkLib = ulib.ProvidesUsesLib()
	}

	depTag := ctx.OtherModuleDependencyTag(depModule)
	if IsLibDepTag(depTag) {
		// Ok, propagate <uses-library> through non-static library dependencies.
	} else if tag, ok := depTag.(usesLibraryDependencyTag); ok && tag.sdkVersion == dexpreopt.AnySdkVersion {
		// Ok, propagate <uses-library> through non-compatibility <uses-library> dependencies.
	} else if depTag == staticLibTag {
		// Propagate <uses-library> through static library dependencies, unless it is a component
		// library (such as stubs). Component libraries have a dependency on their SDK library,
		// which should not be pulled just because of a static component library.
		if sdkLib != nil {
			return
		}
	} else {
		// Don't propagate <uses-library> for other dependency tags.
		return
	}

	// If this is an SDK (or SDK-like) library, then it should be added as a node in the CLC tree,
	// and its CLC should be added as subtree of that node. Otherwise the library is not a
	// <uses_library> and should not be added to CLC, but the transitive <uses-library> dependencies
	// from its CLC should be added to the current CLC.
	if sdkLib != nil {
		clcMap.AddContext(ctx, dexpreopt.AnySdkVersion, *sdkLib, false,
			dep.DexJarBuildPath().PathOrNil(), dep.DexJarInstallPath(), dep.ClassLoaderContexts())
	} else {
		clcMap.AddContextMap(dep.ClassLoaderContexts(), depName)
	}
}

type javaResourcesAttributes struct {
	Resources             bazel.LabelListAttribute
	Resource_strip_prefix *string
	Additional_resources  bazel.LabelListAttribute
}

func (m *Library) getResourceFilegroupStripPrefix(ctx android.TopDownMutatorContext, resourceFilegroup string) (*string, bool) {
	if otherM, ok := ctx.ModuleFromName(resourceFilegroup); ok {
		if fg, isFilegroup := otherM.(android.FileGroupPath); isFilegroup {
			return proptools.StringPtr(filepath.Join(ctx.OtherModuleDir(otherM), fg.GetPath(ctx))), true
		}
	}
	return proptools.StringPtr(""), false
}

func (m *Library) convertJavaResourcesAttributes(ctx android.TopDownMutatorContext) *javaResourcesAttributes {
	var resources bazel.LabelList
	var resourceStripPrefix *string

	additionalJavaResourcesMap := make(map[string]*javaResourcesAttributes)

	if m.properties.Java_resources != nil {
		for _, res := range m.properties.Java_resources {
			if prefix, isFilegroup := m.getResourceFilegroupStripPrefix(ctx, res); isFilegroup {
				otherM, _ := ctx.ModuleFromName(res)
				resourcesTargetName := ctx.ModuleName() + "_filegroup_resources_" + otherM.Name()
				additionalJavaResourcesMap[resourcesTargetName] = &javaResourcesAttributes{
					Resources:             bazel.MakeLabelListAttribute(android.BazelLabelForModuleSrc(ctx, []string{res})),
					Resource_strip_prefix: prefix,
				}
			} else {
				resources.Append(android.BazelLabelForModuleSrc(ctx, []string{res}))
			}
		}

		if !resources.IsEmpty() {
			resourceStripPrefix = proptools.StringPtr(ctx.ModuleDir())
		}
	}

	//TODO(b/179889880) handle case where glob includes files outside package
	resDeps := ResourceDirsToFiles(
		ctx,
		m.properties.Java_resource_dirs,
		m.properties.Exclude_java_resource_dirs,
		m.properties.Exclude_java_resources,
	)

	for _, resDep := range resDeps {
		dir, files := resDep.dir, resDep.files

		// Bazel includes the relative path from the WORKSPACE root when placing the resource
		// inside the JAR file, so we need to remove that prefix
		prefix := proptools.StringPtr(dir.String())
		resourcesTargetName := ctx.ModuleName() + "_resource_dir_" + dir.String()
		additionalJavaResourcesMap[resourcesTargetName] = &javaResourcesAttributes{
			Resources:             bazel.MakeLabelListAttribute(bazel.MakeLabelList(android.RootToModuleRelativePaths(ctx, files))),
			Resource_strip_prefix: prefix,
		}
	}

	var additionalResourceLabels bazel.LabelList
	if len(additionalJavaResourcesMap) > 0 {
		var additionalResources []string
		for resName, _ := range additionalJavaResourcesMap {
			additionalResources = append(additionalResources, resName)
		}
		sort.Strings(additionalResources)

		for i, resName := range additionalResources {
			resAttr := additionalJavaResourcesMap[resName]
			if resourceStripPrefix == nil && i == 0 {
				resourceStripPrefix = resAttr.Resource_strip_prefix
				resources = resAttr.Resources.Value
			} else {
				ctx.CreateBazelTargetModule(
					bazel.BazelTargetModuleProperties{
						Rule_class:        "java_resources",
						Bzl_load_location: "//build/bazel/rules/java:java_resources.bzl",
					},
					android.CommonAttributes{Name: resName},
					resAttr,
				)
				additionalResourceLabels.Append(android.BazelLabelForModuleSrc(ctx, []string{resName}))
			}
		}

	}

	return &javaResourcesAttributes{
		Resources:             bazel.MakeLabelListAttribute(resources),
		Resource_strip_prefix: resourceStripPrefix,
		Additional_resources:  bazel.MakeLabelListAttribute(additionalResourceLabels),
	}
}

type javaCommonAttributes struct {
	*javaResourcesAttributes
	*kotlinAttributes
	Srcs                    bazel.LabelListAttribute
	Plugins                 bazel.LabelListAttribute
	Javacopts               bazel.StringListAttribute
	Sdk_version             bazel.StringAttribute
	Java_version            bazel.StringAttribute
	Errorprone_force_enable bazel.BoolAttribute
}

type javaDependencyLabels struct {
	// Dependencies which DO NOT contribute to the API visible to upstream dependencies.
	Deps bazel.LabelListAttribute
	// Dependencies which DO contribute to the API visible to upstream dependencies.
	StaticDeps bazel.LabelListAttribute
}

type eventLogTagsAttributes struct {
	Srcs bazel.LabelListAttribute
}

type aidlLibraryAttributes struct {
	Srcs bazel.LabelListAttribute
	Tags bazel.StringListAttribute
}

type javaAidlLibraryAttributes struct {
	Deps bazel.LabelListAttribute
	Tags bazel.StringListAttribute
}

// bp2BuildJavaInfo has information needed for the conversion of  java*_modules
// that is needed bor Bp2Build conversion but that requires different handling
// depending on the module type.
type bp2BuildJavaInfo struct {
	// separates dependencies into dynamic dependencies and static dependencies.
	DepLabels *javaDependencyLabels
	hasKotlin bool
}

func javaXsdTargetName(xsd android.XsdConfigBp2buildTargets) string {
	return xsd.JavaBp2buildTargetName()
}

// convertLibraryAttrsBp2Build returns a javaCommonAttributes struct with
// converted attributes shared across java_* modules and a bp2BuildJavaInfo struct
// which has other non-attribute information needed for bp2build conversion
// that needs different handling depending on the module types, and thus needs
// to be returned to the calling function.
func (m *Library) convertLibraryAttrsBp2Build(ctx android.TopDownMutatorContext) (*javaCommonAttributes, *bp2BuildJavaInfo, bool) {
	var srcs bazel.LabelListAttribute
	var deps bazel.LabelListAttribute
	var staticDeps bazel.LabelListAttribute

	if proptools.String(m.deviceProperties.Sdk_version) == "" && m.DeviceSupported() {
		ctx.MarkBp2buildUnconvertible(bp2build_metrics_proto.UnconvertedReasonType_PROPERTY_UNSUPPORTED, "sdk_version unset")
		return &javaCommonAttributes{}, &bp2BuildJavaInfo{}, false
	}

	archVariantProps := m.GetArchVariantProperties(ctx, &CommonProperties{})
	for axis, configToProps := range archVariantProps {
		for config, _props := range configToProps {
			if archProps, ok := _props.(*CommonProperties); ok {
				archSrcs := android.BazelLabelForModuleSrcExcludes(ctx, archProps.Srcs, archProps.Exclude_srcs)
				srcs.SetSelectValue(axis, config, archSrcs)
				if archProps.Jarjar_rules != nil {
					ctx.MarkBp2buildUnconvertible(bp2build_metrics_proto.UnconvertedReasonType_PROPERTY_UNSUPPORTED, "jarjar_rules")
					return &javaCommonAttributes{}, &bp2BuildJavaInfo{}, false
				}
			}
		}
	}
	srcs.ResolveExcludes()

	javaSrcPartition := "java"
	protoSrcPartition := "proto"
	xsdSrcPartition := "xsd"
	logtagSrcPartition := "logtag"
	aidlSrcPartition := "aidl"
	kotlinPartition := "kotlin"
	srcPartitions := bazel.PartitionLabelListAttribute(ctx, &srcs, bazel.LabelPartitions{
		javaSrcPartition:   bazel.LabelPartition{Extensions: []string{".java"}, Keep_remainder: true},
		logtagSrcPartition: bazel.LabelPartition{Extensions: []string{".logtags", ".logtag"}},
		protoSrcPartition:  android.ProtoSrcLabelPartition,
		aidlSrcPartition:   android.AidlSrcLabelPartition,
		xsdSrcPartition:    bazel.LabelPartition{LabelMapper: android.XsdLabelMapper(javaXsdTargetName)},
		kotlinPartition:    bazel.LabelPartition{Extensions: []string{".kt"}},
	})

	javaSrcs := srcPartitions[javaSrcPartition]
	kotlinSrcs := srcPartitions[kotlinPartition]
	javaSrcs.Append(kotlinSrcs)

	staticDeps.Append(srcPartitions[xsdSrcPartition])

	if !srcPartitions[logtagSrcPartition].IsEmpty() {
		logtagsLibName := m.Name() + "_logtags"
		ctx.CreateBazelTargetModule(
			bazel.BazelTargetModuleProperties{
				Rule_class:        "event_log_tags",
				Bzl_load_location: "//build/bazel/rules/java:event_log_tags.bzl",
			},
			android.CommonAttributes{Name: logtagsLibName},
			&eventLogTagsAttributes{
				Srcs: srcPartitions[logtagSrcPartition],
			},
		)

		logtagsSrcs := bazel.MakeLabelList([]bazel.Label{{Label: ":" + logtagsLibName}})
		javaSrcs.Append(bazel.MakeLabelListAttribute(logtagsSrcs))
	}

	if !srcPartitions[aidlSrcPartition].IsEmpty() {
		aidlLibs, aidlSrcs := srcPartitions[aidlSrcPartition].Partition(func(src bazel.Label) bool {
			return android.IsConvertedToAidlLibrary(ctx, src.OriginalModuleName)
		})

		apexAvailableTags := android.ApexAvailableTagsWithoutTestApexes(ctx, ctx.Module())

		if !aidlSrcs.IsEmpty() {
			aidlLibName := m.Name() + "_aidl_library"
			ctx.CreateBazelTargetModule(
				bazel.BazelTargetModuleProperties{
					Rule_class:        "aidl_library",
					Bzl_load_location: "//build/bazel/rules/aidl:aidl_library.bzl",
				},
				android.CommonAttributes{Name: aidlLibName},
				&aidlLibraryAttributes{
					Srcs: aidlSrcs,
					Tags: apexAvailableTags,
				},
			)
			aidlLibs.Add(&bazel.LabelAttribute{Value: &bazel.Label{Label: ":" + aidlLibName}})
		}

		javaAidlLibName := m.Name() + "_java_aidl_library"
		ctx.CreateBazelTargetModule(
			bazel.BazelTargetModuleProperties{
				Rule_class:        "java_aidl_library",
				Bzl_load_location: "//build/bazel/rules/java:java_aidl_library.bzl",
			},
			android.CommonAttributes{Name: javaAidlLibName},
			&javaAidlLibraryAttributes{
				Deps: aidlLibs,
				Tags: apexAvailableTags,
			},
		)

		staticDeps.Append(bazel.MakeSingleLabelListAttribute(bazel.Label{Label: ":" + javaAidlLibName}))
	}

	var javacopts bazel.StringListAttribute //[]string
	plugins := bazel.MakeLabelListAttribute(
		android.BazelLabelForModuleDeps(ctx, m.properties.Plugins),
	)
	if m.properties.Javacflags != nil {
		javacopts = bazel.MakeStringListAttribute(m.properties.Javacflags)
	}

	epEnabled := m.properties.Errorprone.Enabled
	epJavacflags := m.properties.Errorprone.Javacflags
	var errorproneForceEnable bazel.BoolAttribute
	if epEnabled == nil {
		//TODO(b/227504307) add configuration that depends on RUN_ERROR_PRONE environment variable
	} else if *epEnabled {
		plugins.Append(bazel.MakeLabelListAttribute(android.BazelLabelForModuleDeps(ctx, m.properties.Errorprone.Extra_check_modules)))
		javacopts.Append(bazel.MakeStringListAttribute(epJavacflags))
		errorproneForceEnable.Value = epEnabled
	} else {
		javacopts.Append(bazel.MakeStringListAttribute([]string{"-XepDisableAllChecks"}))
	}

	commonAttrs := &javaCommonAttributes{
		Srcs:                    javaSrcs,
		javaResourcesAttributes: m.convertJavaResourcesAttributes(ctx),
		Plugins:                 plugins,
		Javacopts:               javacopts,
		Java_version:            bazel.StringAttribute{Value: m.properties.Java_version},
		Sdk_version:             bazel.StringAttribute{Value: m.deviceProperties.Sdk_version},
		Errorprone_force_enable: errorproneForceEnable,
	}

	for axis, configToProps := range archVariantProps {
		for config, _props := range configToProps {
			if archProps, ok := _props.(*CommonProperties); ok {
				var libLabels []bazel.Label
				for _, d := range archProps.Libs {
					neverlinkLabel := android.BazelLabelForModuleDepSingle(ctx, d)
					neverlinkLabel.Label = neverlinkLabel.Label + "-neverlink"
					libLabels = append(libLabels, neverlinkLabel)
				}
				deps.SetSelectValue(axis, config, bazel.MakeLabelList(libLabels))
			}
		}
	}

	protoDepLabel := bp2buildProto(ctx, &m.Module, srcPartitions[protoSrcPartition])
	// Soong does not differentiate between a java_library and the Bazel equivalent of
	// a java_proto_library + proto_library pair. Instead, in Soong proto sources are
	// listed directly in the srcs of a java_library, and the classes produced
	// by protoc are included directly in the resulting JAR. Thus upstream dependencies
	// that depend on a java_library with proto sources can link directly to the protobuf API,
	// and so this should be a static dependency.
	if protoDepLabel != nil {
		staticDeps.Append(bazel.MakeSingleLabelListAttribute(*protoDepLabel))
	}

	depLabels := &javaDependencyLabels{}
	depLabels.Deps = deps

	for axis, configToProps := range archVariantProps {
		for config, _props := range configToProps {
			if archProps, ok := _props.(*CommonProperties); ok {
				archStaticLibs := android.BazelLabelForModuleDeps(
					ctx,
					android.LastUniqueStrings(android.CopyOf(archProps.Static_libs)))
				depLabels.StaticDeps.SetSelectValue(axis, config, archStaticLibs)
			}
		}
	}
	depLabels.StaticDeps.Append(staticDeps)

	hasKotlin := !kotlinSrcs.IsEmpty()
	commonAttrs.kotlinAttributes = &kotlinAttributes{
		Kotlincflags: &m.properties.Kotlincflags,
	}
	if len(m.properties.Common_srcs) != 0 {
		hasKotlin = true
		commonAttrs.kotlinAttributes.Common_srcs = bazel.MakeLabelListAttribute(android.BazelLabelForModuleSrc(ctx, m.properties.Common_srcs))
	}

	bp2BuildInfo := &bp2BuildJavaInfo{
		DepLabels: depLabels,
		hasKotlin: hasKotlin,
	}

	return commonAttrs, bp2BuildInfo, true
}

type javaLibraryAttributes struct {
	*javaCommonAttributes
	Deps      bazel.LabelListAttribute
	Exports   bazel.LabelListAttribute
	Neverlink bazel.BoolAttribute
}

type kotlinAttributes struct {
	Common_srcs  bazel.LabelListAttribute
	Kotlincflags *[]string
}

func ktJvmLibraryBazelTargetModuleProperties() bazel.BazelTargetModuleProperties {
	return bazel.BazelTargetModuleProperties{
		Rule_class:        "kt_jvm_library",
		Bzl_load_location: "//build/bazel/rules/kotlin:kt_jvm_library.bzl",
	}
}

func javaLibraryBazelTargetModuleProperties() bazel.BazelTargetModuleProperties {
	return bazel.BazelTargetModuleProperties{
		Rule_class:        "java_library",
		Bzl_load_location: "//build/bazel/rules/java:library.bzl",
	}
}

func javaLibraryBp2Build(ctx android.TopDownMutatorContext, m *Library) {
	commonAttrs, bp2BuildInfo, supported := m.convertLibraryAttrsBp2Build(ctx)
	if !supported {
		return
	}
	depLabels := bp2BuildInfo.DepLabels

	deps := depLabels.Deps
	if !commonAttrs.Srcs.IsEmpty() {
		deps.Append(depLabels.StaticDeps) // we should only append these if there are sources to use them
	} else if !deps.IsEmpty() {
		ctx.ModuleErrorf("Module has direct dependencies but no sources. Bazel will not allow this.")
	}
	var props bazel.BazelTargetModuleProperties
	attrs := &javaLibraryAttributes{
		javaCommonAttributes: commonAttrs,
		Deps:                 deps,
		Exports:              depLabels.StaticDeps,
	}
	name := m.Name()

	if !bp2BuildInfo.hasKotlin {
		props = javaLibraryBazelTargetModuleProperties()
	} else {
		props = ktJvmLibraryBazelTargetModuleProperties()
	}

	ctx.CreateBazelTargetModule(props, android.CommonAttributes{Name: name}, attrs)
	neverlinkProp := true
	neverLinkAttrs := &javaLibraryAttributes{
		Exports:   bazel.MakeSingleLabelListAttribute(bazel.Label{Label: ":" + name}),
		Neverlink: bazel.BoolAttribute{Value: &neverlinkProp},
		javaCommonAttributes: &javaCommonAttributes{
			Sdk_version:  bazel.StringAttribute{Value: m.deviceProperties.Sdk_version},
			Java_version: bazel.StringAttribute{Value: m.properties.Java_version},
		},
	}
	ctx.CreateBazelTargetModule(props, android.CommonAttributes{Name: name + "-neverlink"}, neverLinkAttrs)

}

type javaBinaryHostAttributes struct {
	*javaCommonAttributes
	Deps         bazel.LabelListAttribute
	Runtime_deps bazel.LabelListAttribute
	Main_class   string
	Jvm_flags    bazel.StringListAttribute
}

// JavaBinaryHostBp2Build is for java_binary_host bp2build.
func javaBinaryHostBp2Build(ctx android.TopDownMutatorContext, m *Binary) {
	commonAttrs, bp2BuildInfo, supported := m.convertLibraryAttrsBp2Build(ctx)
	if !supported {
		return
	}
	depLabels := bp2BuildInfo.DepLabels

	deps := depLabels.Deps
	deps.Append(depLabels.StaticDeps)
	if m.binaryProperties.Jni_libs != nil {
		deps.Append(bazel.MakeLabelListAttribute(android.BazelLabelForModuleDeps(ctx, m.binaryProperties.Jni_libs)))
	}

	var runtimeDeps bazel.LabelListAttribute
	if commonAttrs.Srcs.IsEmpty() {
		// if there are no sources, then the dependencies can only be used at runtime
		runtimeDeps = deps
		deps = bazel.LabelListAttribute{}
	}

	mainClass := ""
	if m.binaryProperties.Main_class != nil {
		mainClass = *m.binaryProperties.Main_class
	}
	if m.properties.Manifest != nil {
		mainClassInManifest, err := android.GetMainClassInManifest(ctx.Config(), android.PathForModuleSrc(ctx, *m.properties.Manifest).String())
		if err != nil {
			return
		}
		mainClass = mainClassInManifest
	}

	// Attribute jvm_flags
	var jvmFlags bazel.StringListAttribute
	if m.binaryProperties.Jni_libs != nil {
		jniLibPackages := []string{}
		for _, jniLib := range m.binaryProperties.Jni_libs {
			if jniLibModule, exists := ctx.ModuleFromName(jniLib); exists {
				otherDir := ctx.OtherModuleDir(jniLibModule)
				jniLibPackages = append(jniLibPackages, filepath.Join(otherDir, jniLib))
			}
		}
		jniLibPaths := []string{}
		for _, jniLibPackage := range jniLibPackages {
			// See cs/f:.*/third_party/bazel/.*java_stub_template.txt for the use of RUNPATH
			jniLibPaths = append(jniLibPaths, "$${RUNPATH}"+jniLibPackage)
		}
		jvmFlags = bazel.MakeStringListAttribute([]string{"-Djava.library.path=" + strings.Join(jniLibPaths, ":")})
	}

	props := bazel.BazelTargetModuleProperties{
		Rule_class:        "java_binary",
		Bzl_load_location: "@rules_java//java:defs.bzl",
	}
	binAttrs := &javaBinaryHostAttributes{
		Runtime_deps: runtimeDeps,
		Main_class:   mainClass,
		Jvm_flags:    jvmFlags,
	}

	if commonAttrs.Srcs.IsEmpty() {
		binAttrs.javaCommonAttributes = commonAttrs
		ctx.CreateBazelTargetModule(props, android.CommonAttributes{Name: m.Name()}, binAttrs)
		return
	}

	libInfo := libraryCreationInfo{
		deps:      deps,
		attrs:     commonAttrs,
		baseName:  m.Name(),
		hasKotlin: bp2BuildInfo.hasKotlin,
	}
	libName := createLibraryTarget(ctx, libInfo)
	binAttrs.Runtime_deps.Add(&bazel.LabelAttribute{Value: &bazel.Label{Label: ":" + libName}})

	// Create the BazelTargetModule.
	ctx.CreateBazelTargetModule(props, android.CommonAttributes{Name: m.Name()}, binAttrs)
}

type javaTestHostAttributes struct {
	*javaCommonAttributes
	Srcs         bazel.LabelListAttribute
	Deps         bazel.LabelListAttribute
	Runtime_deps bazel.LabelListAttribute
}

// javaTestHostBp2Build is for java_test_host bp2build.
func javaTestHostBp2Build(ctx android.TopDownMutatorContext, m *TestHost) {
	commonAttrs, bp2BuildInfo, supported := m.convertLibraryAttrsBp2Build(ctx)
	if !supported {
		return
	}
	depLabels := bp2BuildInfo.DepLabels

	deps := depLabels.Deps
	deps.Append(depLabels.StaticDeps)

	var runtimeDeps bazel.LabelListAttribute
	attrs := &javaTestHostAttributes{
		Runtime_deps: runtimeDeps,
	}
	props := bazel.BazelTargetModuleProperties{
		Rule_class:        "java_test",
		Bzl_load_location: "//build/bazel/rules/java:test.bzl",
	}

	if commonAttrs.Srcs.IsEmpty() {
		// if there are no sources, then the dependencies can only be used at runtime
		attrs.Runtime_deps = deps
		attrs.javaCommonAttributes = commonAttrs
		ctx.CreateBazelTargetModule(props, android.CommonAttributes{Name: m.Name()}, attrs)
		return
	}

	libInfo := libraryCreationInfo{
		deps:      deps,
		attrs:     commonAttrs,
		baseName:  m.Name(),
		hasKotlin: bp2BuildInfo.hasKotlin,
	}
	libName := createLibraryTarget(ctx, libInfo)

	attrs.Srcs = commonAttrs.Srcs
	attrs.Deps = deps
	attrs.Runtime_deps.Add(&bazel.LabelAttribute{Value: &bazel.Label{Label: ":" + libName}})
	// Create the BazelTargetModule.
	ctx.CreateBazelTargetModule(props, android.CommonAttributes{Name: m.Name()}, attrs)
}

// libraryCreationInfo encapsulates the info needed to create java_library target from
// java_binary_host or java_test_host.
type libraryCreationInfo struct {
	deps      bazel.LabelListAttribute
	attrs     *javaCommonAttributes
	baseName  string
	hasKotlin bool
}

// helper function that creates java_library target from java_binary_host or java_test_host,
// and returns the library target name,
func createLibraryTarget(ctx android.TopDownMutatorContext, libInfo libraryCreationInfo) string {
	libName := libInfo.baseName + "_lib"
	var libProps bazel.BazelTargetModuleProperties
	if libInfo.hasKotlin {
		libProps = ktJvmLibraryBazelTargetModuleProperties()
	} else {
		libProps = javaLibraryBazelTargetModuleProperties()
	}
	libAttrs := &javaLibraryAttributes{
		Deps:                 libInfo.deps,
		javaCommonAttributes: libInfo.attrs,
	}

	ctx.CreateBazelTargetModule(libProps, android.CommonAttributes{Name: libName}, libAttrs)
	return libName
}

type bazelJavaImportAttributes struct {
	Jars    bazel.LabelListAttribute
	Exports bazel.LabelListAttribute
}

// java_import bp2Build converter.
func (i *Import) ConvertWithBp2build(ctx android.TopDownMutatorContext) {
	var jars bazel.LabelListAttribute
	archVariantProps := i.GetArchVariantProperties(ctx, &ImportProperties{})
	for axis, configToProps := range archVariantProps {
		for config, _props := range configToProps {
			if archProps, ok := _props.(*ImportProperties); ok {
				archJars := android.BazelLabelForModuleSrcExcludes(ctx, archProps.Jars, []string(nil))
				jars.SetSelectValue(axis, config, archJars)
			}
		}
	}

	attrs := &bazelJavaImportAttributes{
		Jars: jars,
	}
	props := bazel.BazelTargetModuleProperties{
		Rule_class:        "java_import",
		Bzl_load_location: "//build/bazel/rules/java:import.bzl",
	}

	name := android.RemoveOptionalPrebuiltPrefix(i.Name())

	ctx.CreateBazelTargetModule(props, android.CommonAttributes{Name: name}, attrs)

	neverlink := true
	neverlinkAttrs := &javaLibraryAttributes{
		Neverlink: bazel.BoolAttribute{Value: &neverlink},
		Exports:   bazel.MakeSingleLabelListAttribute(bazel.Label{Label: ":" + name}),
		javaCommonAttributes: &javaCommonAttributes{
			Sdk_version: bazel.StringAttribute{Value: proptools.StringPtr("none")},
		},
	}
	ctx.CreateBazelTargetModule(
		javaLibraryBazelTargetModuleProperties(),
		android.CommonAttributes{Name: name + "-neverlink"},
		neverlinkAttrs)

}

var _ android.MixedBuildBuildable = (*Import)(nil)

func (i *Import) getBazelModuleLabel(ctx android.BaseModuleContext) string {
	return android.RemoveOptionalPrebuiltPrefixFromBazelLabel(i.GetBazelLabel(ctx, i))
}

func (i *Import) ProcessBazelQueryResponse(ctx android.ModuleContext) {
	i.commonBuildActions(ctx)

	bazelCtx := ctx.Config().BazelContext
	filePaths, err := bazelCtx.GetOutputFiles(i.getBazelModuleLabel(ctx), android.GetConfigKey(ctx))
	if err != nil {
		ctx.ModuleErrorf(err.Error())
		return
	}

	bazelJars := android.Paths{}
	for _, bazelOutputFile := range filePaths {
		bazelJars = append(bazelJars, android.PathForBazelOut(ctx, bazelOutputFile))
	}

	jarName := android.RemoveOptionalPrebuiltPrefix(i.Name()) + ".jar"
	outputFile := android.PathForModuleOut(ctx, "bazelCombined", jarName)
	TransformJarsToJar(ctx, outputFile, "combine prebuilt jars", bazelJars,
		android.OptionalPath{}, // manifest
		false,                  // stripDirEntries
		[]string{},             // filesToStrip
		[]string{},             // dirsToStrip
	)
	i.combinedClasspathFile = outputFile

	ctx.SetProvider(JavaInfoProvider, JavaInfo{
		HeaderJars:                     android.PathsIfNonNil(i.combinedClasspathFile),
		ImplementationAndResourcesJars: android.PathsIfNonNil(i.combinedClasspathFile),
		ImplementationJars:             android.PathsIfNonNil(i.combinedClasspathFile),
		// TODO(b/240308299) include AIDL information from Bazel
		// TODO: aconfig files?
	})

	i.maybeInstall(ctx, jarName, outputFile)
}

func (i *Import) QueueBazelCall(ctx android.BaseModuleContext) {
	bazelCtx := ctx.Config().BazelContext
	bazelCtx.QueueBazelRequest(i.getBazelModuleLabel(ctx), cquery.GetOutputFiles, android.GetConfigKey(ctx))
}

func (i *Import) IsMixedBuildSupported(ctx android.BaseModuleContext) bool {
	return true
}
